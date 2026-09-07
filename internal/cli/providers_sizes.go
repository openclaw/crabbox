package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

func (a App) providerSizes(ctx context.Context, args []string) error {
	fs := newFlagSet("providers sizes", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	refresh := fs.Bool("refresh", false, "bypass any provider catalog cache")
	includeUnavailable := fs.Bool("all", false, "include sizes with no currently available regions")
	withContext := fs.Bool("with-context", false, "include effective native selection context; requires --json")
	class := fs.String("class", "", "machine class for effective selection")
	if err := parseInterspersedFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return exit(2, "usage: crabbox providers sizes <provider> [--all] [--refresh] [--class CLASS] [--json] [--with-context]")
	}
	if *withContext && !*jsonOut {
		return exit(2, "--with-context requires --json")
	}
	provider, err := ProviderFor(fs.Arg(0))
	if err != nil {
		return err
	}
	if *withContext && provider.Spec().SizeSelection == "" {
		return exit(2, "provider=%s does not expose native size selection", provider.Name())
	}
	cfg, err := loadConfigWithOverrides("", provider.Name())
	if err != nil {
		return err
	}
	if flagWasSet(fs, "class") {
		cfg.Class = *class
		MarkClassExplicit(&cfg)
	}
	applyServerTypeFlagOverrides(&cfg, fs, "")
	backend, err := loadBackend(cfg, runtimeForApp(a))
	if err != nil {
		return err
	}
	catalog, ok := backend.(ProviderSizeCatalogBackend)
	if !ok {
		return exit(2, "provider=%s does not expose a live size catalog", provider.Name())
	}
	var selection ProviderSizeSelection
	if *withContext {
		selector, ok := backend.(ProviderSizeSelectionBackend)
		if !ok {
			return exit(2, "provider=%s does not expose native size selection", provider.Name())
		}
		selection = selector.SizeSelection()
	}
	sizes, err := catalog.SizeCatalog(ctx, *refresh)
	if err != nil {
		return err
	}
	if !*includeUnavailable {
		filtered := sizes[:0]
		for _, size := range sizes {
			if len(size.Regions) > 0 {
				filtered = append(filtered, size)
			}
		}
		sizes = filtered
	}
	if *jsonOut {
		if *withContext {
			return json.NewEncoder(a.Stdout).Encode(struct {
				Sizes     []ProviderSize        `json:"sizes"`
				Selection ProviderSizeSelection `json:"selection"`
			}{sizes, selection})
		}
		return json.NewEncoder(a.Stdout).Encode(sizes)
	}
	fmt.Fprintln(a.Stdout, "SIZE\tCPU\tGPU\tRAM_GB\tDISK_GB\tPRICE_PER_HOUR\tREGIONS")
	for _, size := range sizes {
		gpu := "-"
		if size.GPU != nil {
			gpu = size.GPU.Label
		}
		fmt.Fprintf(a.Stdout, "%s\t%d\t%s\t%d\t%d\t%s\t%s\n", size.Name, size.VCPU, gpu, size.RAMGB, size.DiskGB, providerHourlyMicro(size.PricePerHourMicro), strings.Join(size.Regions, ","))
	}
	return nil
}

func providerHourlyMicro(value int64) string {
	amount := strconv.FormatFloat(float64(value)/1_000_000, 'f', 6, 64)
	amount = strings.TrimRight(strings.TrimRight(amount, "0"), ".")
	if amount == "" {
		amount = "0"
	}
	return "$" + amount
}
