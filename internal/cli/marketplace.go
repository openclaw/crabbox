package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (a App) marketplaceStatus(ctx context.Context, args []string) error {
	fs := newFlagSet("marketplace status", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := marketplaceCoordinator()
	if err != nil {
		return err
	}
	res, err := coord.MarketplaceStatus(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	printMarketplaceStatus(a.Stdout, res)
	return nil
}

func (a App) marketplaceQuote(ctx context.Context, args []string) error {
	fs := newFlagSet("marketplace quote", a.Stderr)
	provider := fs.String("provider", "auto", "provider or auto")
	providers := fs.String("providers", "", "comma-separated provider candidates")
	className := fs.String("class", "standard", "machine class")
	serverType := fs.String("type", "", "provider-native server type or marketplace SKU")
	target := fs.String("target", "linux", "target OS")
	ttl := fs.String("ttl", "1h", "quote TTL, e.g. 30m, 1h, or 3600s")
	maxCredits := fs.Float64("max-credits", 0, "maximum credits to spend")
	strategy := fs.String("strategy", "cheapest", "cheapest, balanced, or provider-default")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	ttlDuration, err := parseMarketplaceTTL(*ttl)
	if err != nil {
		return err
	}
	input := CoordinatorMarketplaceQuoteRequest{
		Provider:   strings.TrimSpace(*provider),
		Providers:  splitMarketplaceList(*providers),
		Class:      strings.TrimSpace(*className),
		ServerType: strings.TrimSpace(*serverType),
		Target:     strings.TrimSpace(*target),
		TTLSeconds: int(ttlDuration.Seconds()),
		Strategy:   strings.TrimSpace(*strategy),
	}
	if *maxCredits > 0 {
		input.MaxCredits = *maxCredits
	}
	coord, err := marketplaceCoordinator()
	if err != nil {
		return err
	}
	res, err := coord.MarketplaceQuote(ctx, input)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	printMarketplaceQuote(a.Stdout, res)
	return nil
}

func marketplaceCoordinator() (*CoordinatorClient, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, exit(2, "marketplace requires a configured coordinator")
	}
	return coord, nil
}

func printMarketplaceStatus(out interface{ Write([]byte) (int, error) }, res CoordinatorMarketplaceStatusResponse) {
	marketplace := res.Marketplace
	fmt.Fprintf(out, "marketplace mode=%s enabled=%t currency=%s credit_unit=%s\n",
		marketplace.Mode, marketplace.Enabled, marketplace.Currency, marketplace.CreditUnit)
	if res.Owner != "" || res.Org != "" {
		fmt.Fprintf(out, "identity owner=%s org=%s\n", marketplaceFallback(res.Owner, "unknown"), marketplaceFallback(res.Org, "unknown"))
	}
	fmt.Fprintf(out, "features quotes=%s bidding=%s payments=%s ledger=%s enforcement=%s\n",
		onOff(marketplace.Features.Quotes),
		onOff(marketplace.Features.Bidding),
		onOff(marketplace.Features.Payments),
		onOff(marketplace.Features.CreditLedger),
		onOff(marketplace.Features.LeaseEnforcement))
	fmt.Fprintf(out, "credits required_for_leases=%s\n", onOff(marketplace.RequireCreditsForLeases))
	if len(marketplace.SupportedProviders) > 0 {
		fmt.Fprintf(out, "providers %s\n", strings.Join(marketplace.SupportedProviders, ","))
	}
	fmt.Fprintf(out, "settlement payment=%s ledger=%s provider=%s\n",
		marketplaceFallback(marketplace.Settlement.PaymentProvider, "none"),
		marketplaceFallback(marketplace.Settlement.LedgerProvider, "none"),
		marketplaceFallback(marketplace.Settlement.ProviderSettlement, "external"))
	printMarketplaceDecisions(out, marketplace.DecisionsRequired)
}

func printMarketplaceQuote(out interface{ Write([]byte) (int, error) }, res CoordinatorMarketplaceQuoteResponse) {
	quote := res.Quote
	fmt.Fprintf(out, "marketplace quote %s mode=%s strategy=%s ttl=%s\n",
		quote.ID, quote.Mode, quote.Strategy, formatDurationSeconds(int64(quote.TTLSeconds)))
	if quote.Selected != nil {
		candidate := *quote.Selected
		fmt.Fprintf(out, "selected provider=%s route=%s priority=%d weight=%.2f credits=$%.2f retail=$%.2f/h provider_cost=$%.2f/h margin=$%.2f\n",
			candidate.Provider,
			candidate.RouteKey,
			candidate.Priority,
			candidate.Weight,
			candidate.Credits,
			candidate.RetailHourlyUSD,
			candidate.CostHourlyUSD,
			candidate.MarginUSD)
	} else {
		fmt.Fprintln(out, "selected none")
	}
	if len(quote.Candidates) > 0 {
		fmt.Fprintln(out, "candidates:")
		for _, candidate := range quote.Candidates {
			status := "available"
			if !candidate.Available {
				status = "unavailable"
				if candidate.UnavailableReason != "" {
					status += ":" + candidate.UnavailableReason
				}
			}
			fmt.Fprintf(out, "  %-8s priority=%-3d weight=%-6.2f credits=$%-8.2f retail=$%-8.2f/h provider_cost=$%-8.2f/h route=%-28s %s\n",
				candidate.Provider,
				candidate.Priority,
				candidate.Weight,
				candidate.Credits,
				candidate.RetailHourlyUSD,
				candidate.CostHourlyUSD,
				candidate.RouteKey,
				status)
		}
	}
	printMarketplaceWarnings(out, quote.Warnings)
	printMarketplaceDecisions(out, res.Marketplace.DecisionsRequired)
}

func printMarketplaceWarnings(out interface{ Write([]byte) (int, error) }, warnings []string) {
	if len(warnings) == 0 {
		return
	}
	fmt.Fprintln(out, "warnings:")
	for _, warning := range warnings {
		fmt.Fprintf(out, "  - %s\n", warning)
	}
}

func printMarketplaceDecisions(out interface{ Write([]byte) (int, error) }, decisions []string) {
	if len(decisions) == 0 {
		return
	}
	fmt.Fprintln(out, "decisions_required:")
	for _, decision := range decisions {
		fmt.Fprintf(out, "  - %s\n", decision)
	}
}

func parseMarketplaceTTL(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, exit(2, "ttl must not be empty")
	}
	duration, err := time.ParseDuration(value)
	if err == nil && duration > 0 {
		return duration, nil
	}
	seconds, secondsErr := strconv.Atoi(value)
	if secondsErr == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second, nil
	}
	if err != nil {
		return 0, exit(2, "invalid ttl %q: use values like 30m, 1h, or 3600s", value)
	}
	return 0, exit(2, "ttl must be positive")
}

func splitMarketplaceList(value string) []string {
	var values []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			values = append(values, item)
		}
	}
	return values
}

func marketplaceFallback(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func onOff(value bool) string {
	if value {
		return "on"
	}
	return "off"
}
