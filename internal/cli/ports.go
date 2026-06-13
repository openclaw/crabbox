package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"strings"
)

func (a App) ports(ctx context.Context, args []string) error {
	defaults := defaultConfig()
	fs := newFlagSet("ports", a.Stderr)
	provider := fs.String("provider", defaults.Provider, providerHelpAll())
	id := fs.String("id", "", "lease id or slug")
	jsonOut := fs.Bool("json", false, "print JSON")
	var publish stringListFlag
	var unpublish stringListFlag
	fs.Var(&publish, "publish", "publish a port mapping; repeatable")
	fs.Var(&unpublish, "unpublish", "unpublish a port mapping; repeatable")
	providerFlags := registerProviderFlags(fs, defaults)
	targetFlags := registerTargetFlags(fs, defaults)
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	idFlagSet := flagWasSet(fs, "id")
	setIDFromFirstArg(fs, id)
	if strings.TrimSpace(*id) == "" || fs.NArg() > 1 || (idFlagSet && fs.NArg() > 0) {
		return exit(2, "usage: crabbox ports --id <lease-id-or-slug> [--publish <spec>] [--unpublish <spec>] [--json]")
	}
	if len(publish) > 0 && len(unpublish) > 0 {
		return exit(2, "--publish and --unpublish cannot be combined")
	}
	cfg, err := loadPortsConfig(fs, *provider, providerFlags, targetFlags, *id)
	if err != nil {
		return err
	}
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	portsBackend, ok := backend.(PortsBackend)
	if !ok {
		return exit(2, "provider=%s does not support ports; use a provider with native port publishing", backend.Spec().Name)
	}
	output, err := portsBackend.Ports(ctx, PortsRequest{
		Options:   leaseOptionsFromConfig(cfg),
		ID:        *id,
		Publish:   append([]string(nil), publish...),
		Unpublish: append([]string(nil), unpublish...),
		JSON:      *jsonOut,
	})
	if err != nil {
		return err
	}
	if *jsonOut {
		var value any
		if strings.TrimSpace(output) == "" {
			value = []any{}
		} else if err := json.Unmarshal([]byte(output), &value); err != nil {
			return err
		}
		enc := json.NewEncoder(a.Stdout)
		enc.SetEscapeHTML(false)
		return enc.Encode(value)
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fmt.Fprintln(a.Stdout, line)
	}
	return nil
}

func loadPortsConfig(fs *flag.FlagSet, provider string, providerFlags providerFlagValues, targetFlags targetFlagValues, id string) (Config, error) {
	cfg, err := loadConfig()
	if err != nil {
		return Config{}, err
	}
	if err := prepareProviderSelection(&cfg, provider); err != nil {
		return Config{}, err
	}
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return Config{}, err
	}
	if err := autoRouteStaticLease(&cfg, fs, id); err != nil {
		return Config{}, err
	}
	if err := autoRouteExternalLease(&cfg, fs, id); err != nil {
		return Config{}, err
	}
	if err := applyProviderFlags(&cfg, fs, providerFlags); err != nil {
		return Config{}, err
	}
	if err := finalizeProviderSelection(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}
