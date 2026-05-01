package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

func (a App) config(_ context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox config show|path|set-broker")
	}
	switch args[0] {
	case "path":
		path := userConfigPath()
		if path == "" {
			return exit(2, "user config directory is unavailable")
		}
		fmt.Fprintln(a.Stdout, path)
		return nil
	case "show":
		return a.configShow(args[1:])
	case "set-broker":
		return a.configSetBroker(args[1:])
	default:
		return exit(2, "unknown config command %q", args[0])
	}
}

func (a App) configShow(args []string) error {
	fs := newFlagSet("config show", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	view := map[string]any{
		"profile":     cfg.Profile,
		"provider":    cfg.Provider,
		"class":       cfg.Class,
		"serverType":  cfg.ServerType,
		"coordinator": cfg.Coordinator,
		"brokerAuth":  tokenState(cfg.CoordToken),
		"sshKey":      cfg.SSHKey,
		"sshUser":     cfg.SSHUser,
		"sshPort":     cfg.SSHPort,
		"workRoot":    cfg.WorkRoot,
		"sync": map[string]any{
			"exclude":     configuredExcludes(cfg),
			"delete":      cfg.Sync.Delete,
			"checksum":    cfg.Sync.Checksum,
			"gitSeed":     cfg.Sync.GitSeed,
			"fingerprint": cfg.Sync.Fingerprint,
			"baseRef":     cfg.Sync.BaseRef,
			"timeout":     cfg.Sync.Timeout.String(),
			"warnFiles":   cfg.Sync.WarnFiles,
			"warnBytes":   cfg.Sync.WarnBytes,
			"failFiles":   cfg.Sync.FailFiles,
			"failBytes":   cfg.Sync.FailBytes,
			"allowLarge":  cfg.Sync.AllowLarge,
		},
		"env": map[string]any{
			"allow": cfg.EnvAllow,
		},
		"capacity": map[string]any{
			"market":            cfg.Capacity.Market,
			"strategy":          cfg.Capacity.Strategy,
			"fallback":          cfg.Capacity.Fallback,
			"regions":           cfg.Capacity.Regions,
			"availabilityZones": cfg.Capacity.AvailabilityZones,
		},
		"actions": map[string]any{
			"repo":          cfg.Actions.Repo,
			"workflow":      cfg.Actions.Workflow,
			"job":           cfg.Actions.Job,
			"ref":           cfg.Actions.Ref,
			"runnerLabels":  cfg.Actions.RunnerLabels,
			"runnerVersion": cfg.Actions.RunnerVersion,
			"ephemeral":     cfg.Actions.Ephemeral,
		},
		"blacksmith": map[string]any{
			"org":         cfg.Blacksmith.Org,
			"workflow":    cfg.Blacksmith.Workflow,
			"job":         cfg.Blacksmith.Job,
			"ref":         cfg.Blacksmith.Ref,
			"idleTimeout": cfg.Blacksmith.IdleTimeout.String(),
			"debug":       cfg.Blacksmith.Debug,
		},
		"results": map[string]any{
			"junit": cfg.Results.JUnit,
		},
		"cache": map[string]any{
			"pnpm":           cfg.Cache.Pnpm,
			"npm":            cfg.Cache.Npm,
			"docker":         cfg.Cache.Docker,
			"git":            cfg.Cache.Git,
			"maxGB":          cfg.Cache.MaxGB,
			"purgeOnRelease": cfg.Cache.PurgeOnRelease,
		},
		"hetzner": map[string]any{
			"location": cfg.Location,
			"image":    cfg.Image,
			"sshKey":   cfg.ProviderKey,
		},
		"aws": map[string]any{
			"region":          cfg.AWSRegion,
			"ami":             cfg.AWSAMI,
			"securityGroupId": cfg.AWSSGID,
			"subnetId":        cfg.AWSSubnetID,
			"instanceProfile": cfg.AWSProfile,
			"rootGB":          cfg.AWSRootGB,
			"sshCIDRs":        cfg.AWSSSHCIDRs,
		},
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(view)
	}
	fmt.Fprintf(a.Stdout, "config=%s\n", userConfigPath())
	fmt.Fprintf(a.Stdout, "provider=%s class=%s type=%s profile=%s\n", cfg.Provider, cfg.Class, cfg.ServerType, cfg.Profile)
	fmt.Fprintf(a.Stdout, "broker=%s auth=%s\n", blank(cfg.Coordinator, "-"), tokenState(cfg.CoordToken))
	fmt.Fprintf(a.Stdout, "ssh=%s@<host>:%s key=%s\n", cfg.SSHUser, cfg.SSHPort, cfg.SSHKey)
	fmt.Fprintf(a.Stdout, "sync delete=%t checksum=%t git_seed=%t fingerprint=%t base_ref=%s excludes=%d timeout=%s\n", cfg.Sync.Delete, cfg.Sync.Checksum, cfg.Sync.GitSeed, cfg.Sync.Fingerprint, blank(cfg.Sync.BaseRef, "-"), len(configuredExcludes(cfg)), cfg.Sync.Timeout)
	fmt.Fprintf(a.Stdout, "env allow=%s\n", strings.Join(cfg.EnvAllow, ","))
	fmt.Fprintf(a.Stdout, "capacity market=%s strategy=%s fallback=%s regions=%s\n", cfg.Capacity.Market, cfg.Capacity.Strategy, cfg.Capacity.Fallback, blank(strings.Join(cfg.Capacity.Regions, ","), "-"))
	fmt.Fprintf(a.Stdout, "actions repo=%s workflow=%s job=%s ref=%s runner_version=%s ephemeral=%t labels=%s\n", blank(cfg.Actions.Repo, "-"), blank(cfg.Actions.Workflow, "-"), blank(cfg.Actions.Job, "-"), blank(cfg.Actions.Ref, "-"), cfg.Actions.RunnerVersion, cfg.Actions.Ephemeral, blank(strings.Join(cfg.Actions.RunnerLabels, ","), "-"))
	fmt.Fprintf(a.Stdout, "blacksmith org=%s workflow=%s job=%s ref=%s idle_timeout=%s debug=%t\n", blank(cfg.Blacksmith.Org, "-"), blank(cfg.Blacksmith.Workflow, "-"), blank(cfg.Blacksmith.Job, "-"), blank(cfg.Blacksmith.Ref, "-"), cfg.Blacksmith.IdleTimeout, cfg.Blacksmith.Debug)
	fmt.Fprintf(a.Stdout, "results junit=%s\n", blank(strings.Join(cfg.Results.JUnit, ","), "-"))
	fmt.Fprintf(a.Stdout, "cache pnpm=%t npm=%t docker=%t git=%t max_gb=%d purge_on_release=%t\n", cfg.Cache.Pnpm, cfg.Cache.Npm, cfg.Cache.Docker, cfg.Cache.Git, cfg.Cache.MaxGB, cfg.Cache.PurgeOnRelease)
	fmt.Fprintf(a.Stdout, "aws region=%s root_gb=%d ssh_cidrs=%s\n", cfg.AWSRegion, cfg.AWSRootGB, blank(strings.Join(cfg.AWSSSHCIDRs, ","), "-"))
	return nil
}

func (a App) configSetBroker(args []string) error {
	fs := newFlagSet("config set-broker", a.Stderr)
	url := fs.String("url", "", "broker URL")
	provider := fs.String("provider", "", "default provider: hetzner or aws")
	tokenStdin := fs.Bool("token-stdin", false, "read broker token from stdin")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *url == "" {
		return exit(2, "config set-broker requires --url")
	}
	var token string
	if *tokenStdin {
		data, err := io.ReadAll(os.Stdin)
		if err != nil {
			return exit(2, "read broker token: %v", err)
		}
		token = strings.TrimSpace(string(data))
		if token == "" {
			return exit(2, "broker token from stdin is empty")
		}
	}
	path := writableConfigPath()
	if path == "" {
		return exit(2, "user config directory is unavailable")
	}
	file, err := readFileConfig(path)
	if err != nil {
		return err
	}
	if file.Broker == nil {
		file.Broker = &fileBrokerConfig{}
	}
	file.Broker.URL = *url
	if token != "" {
		file.Broker.Token = token
	}
	if *provider != "" {
		file.Broker.Provider = *provider
		file.Provider = *provider
	}
	written, err := writeUserFileConfig(file)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stdout, "wrote %s broker=%s auth=%s\n", written, *url, tokenState(file.Broker.Token))
	return nil
}

func tokenState(token string) string {
	if token == "" {
		return "missing"
	}
	return "configured"
}

func blank(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
