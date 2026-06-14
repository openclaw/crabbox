package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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
	strategy := fs.String("strategy", "cheapest", "cheapest, balanced, weighted, or provider-default")
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
	// integer display percents per route, allocated by largest remainder so each tier reads 100%
	pct := marketplaceRoutePercents(quote.RoutingPlan)
	fmt.Fprintf(out, "marketplace quote %s mode=%s strategy=%s ttl=%s\n",
		quote.ID, quote.Mode, quote.Strategy, formatDurationSeconds(int64(quote.TTLSeconds)))
	if quote.Selected != nil {
		candidate := *quote.Selected
		fmt.Fprintf(out, "selected provider=%s route=%s priority=%d weight=%.2f credits=$%.2f retail=$%.2f/h provider_cost=$%.2f/h margin=$%.2f%s\n",
			candidate.Provider,
			candidate.RouteKey,
			candidate.Priority,
			candidate.Weight,
			candidate.Credits,
			candidate.RetailHourlyUSD,
			candidate.CostHourlyUSD,
			candidate.MarginUSD,
			marketplaceShareSuffix(candidate.RouteKey, candidate.RouteShare, pct))
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
			fmt.Fprintf(out, "  %-8s priority=%-3d weight=%-6.2f credits=$%-8.2f retail=$%-8.2f/h provider_cost=$%-8.2f/h route=%-28s %s%s\n",
				candidate.Provider,
				candidate.Priority,
				candidate.Weight,
				candidate.Credits,
				candidate.RetailHourlyUSD,
				candidate.CostHourlyUSD,
				candidate.RouteKey,
				status,
				marketplaceShareSuffix(candidate.RouteKey, candidate.RouteShare, pct))
		}
	}
	printMarketplaceRoutingPlan(out, quote.RoutingPlan, pct)
	printMarketplaceWarnings(out, quote.Warnings)
	printMarketplaceDecisions(out, res.Marketplace.DecisionsRequired)
}

func printMarketplaceRoutingPlan(out interface{ Write([]byte) (int, error) }, plan []CoordinatorMarketplaceRouteTier, pct map[string]int) {
	if len(plan) == 0 {
		return
	}
	fmt.Fprintln(out, "routing plan (failover order; preview only, no traffic routed):")
	for _, tier := range plan {
		role := "failover"
		if tier.Active {
			role = "active"
		}
		members := make([]string, 0, len(tier.Members))
		for _, member := range tier.Members {
			members = append(members, fmt.Sprintf("%s %d%%", member.Provider, pct[member.RouteKey]))
		}
		fmt.Fprintf(out, "  tier priority=%-3d [%-8s] %s\n", tier.Priority, role, strings.Join(members, " | "))
	}
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

// marketplaceShareSuffix renders the weighted load-balancing share (e.g. " share=75%"). When a
// routing plan is present it uses the largest-remainder integer percents (which total 100% per tier);
// otherwise it falls back to rounding the raw 0..1 share. It stays empty when no split applies.
func marketplaceShareSuffix(routeKey string, share float64, pct map[string]int) string {
	if value, ok := pct[routeKey]; ok {
		return fmt.Sprintf(" share=%d%%", value)
	}
	if share <= 0 {
		return ""
	}
	return fmt.Sprintf(" share=%.0f%%", share*100)
}

// marketplaceRoutePercents converts each tier's 0..1 routeShares into integer percents using
// largest-remainder allocation, so the displayed percentages within a tier always sum to exactly 100.
func marketplaceRoutePercents(plan []CoordinatorMarketplaceRouteTier) map[string]int {
	percents := map[string]int{}
	for _, tier := range plan {
		n := len(tier.Members)
		if n == 0 {
			continue
		}
		base := make([]int, n)
		sum := 0
		for i, member := range tier.Members {
			base[i] = int(math.Floor(member.RouteShare * 100))
			sum += base[i]
		}
		residual := 100 - sum
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}
		// hand leftover percent points to the largest fractional parts (stable for determinism)
		sort.SliceStable(order, func(a, b int) bool {
			fa := tier.Members[order[a]].RouteShare*100 - math.Floor(tier.Members[order[a]].RouteShare*100)
			fb := tier.Members[order[b]].RouteShare*100 - math.Floor(tier.Members[order[b]].RouteShare*100)
			return fa > fb
		})
		for k := 0; k < residual && k < n; k++ {
			base[order[k]]++
		}
		for i, member := range tier.Members {
			percents[member.RouteKey] = base[i]
		}
	}
	return percents
}
