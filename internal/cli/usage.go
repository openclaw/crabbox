package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a App) usage(ctx context.Context, args []string) error {
	fs := newFlagSet("usage", a.Stderr)
	scope := fs.String("scope", "user", "scope: user, org, or all")
	owner := fs.String("user", "", "owner identity")
	org := fs.String("org", "", "org name")
	month := fs.String("month", time.Now().UTC().Format("2006-01"), "month: YYYY-MM")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *scope != "user" && *scope != "org" && *scope != "all" {
		return exit(2, "usage scope must be user, org, or all")
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	coord, ok, err := newCoordinatorClient(cfg)
	if err != nil {
		return err
	}
	if !ok {
		return exit(2, "usage requires a configured coordinator")
	}
	res, err := coord.Usage(ctx, *scope, *owner, *org, *month)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	printUsageSummary(a.Stdout, res.Usage)
	printCostLimits(a.Stdout, res.Limits)
	return nil
}

func printUsageSummary(out interface{ Write([]byte) (int, error) }, usage CoordinatorUsageSummary) {
	scopeParts := []string{"scope=" + usage.Scope}
	if usage.Owner != "" {
		scopeParts = append(scopeParts, "user="+usage.Owner)
	}
	if usage.Org != "" {
		scopeParts = append(scopeParts, "org="+usage.Org)
	}
	fmt.Fprintf(out, "usage month=%s %s\n", usage.Month, strings.Join(scopeParts, " "))
	fmt.Fprintf(out, "total leases=%d active=%d runtime=%s estimated=$%.2f reserved=$%.2f\n",
		usage.Leases, usage.ActiveLeases, formatDurationSeconds(usage.RuntimeSeconds), usage.EstimatedUSD, usage.ReservedUSD)
	printUsageGroups(out, "owners", usage.ByOwner)
	printUsageGroups(out, "orgs", usage.ByOrg)
	printUsageGroups(out, "providers", usage.ByProvider)
	printUsageGroups(out, "server_types", usage.ByServerType)
}

func printUsageGroups(out interface{ Write([]byte) (int, error) }, title string, groups []CoordinatorUsageGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintf(out, "%s:\n", title)
	for _, group := range groups {
		fmt.Fprintf(out, "  %-24s leases=%-3d active=%-3d runtime=%-9s estimated=$%-8.2f reserved=$%.2f\n",
			group.Key, group.Leases, group.ActiveLeases, formatDurationSeconds(group.RuntimeSeconds), group.EstimatedUSD, group.ReservedUSD)
	}
}

func printCostLimits(out interface{ Write([]byte) (int, error) }, limits CoordinatorCostLimits) {
	fmt.Fprintln(out, "limits:")
	fmt.Fprintf(out, "  active leases: fleet=%s user=%s org=%s\n",
		formatIntLimit(limits.MaxActiveLeases), formatIntLimit(limits.MaxActiveLeasesPerOwner), formatIntLimit(limits.MaxActiveLeasesPerOrg))
	if limits.MaxActiveLeasesPerCapacityAdmin > 0 || len(limits.CapacityAdminOwners) > 0 {
		fmt.Fprintf(out, "  capacity admin: user=%s owners=%d\n",
			formatIntLimit(limits.MaxActiveLeasesPerCapacityAdmin), len(limits.CapacityAdminOwners))
	}
	fmt.Fprintf(out, "  monthly usd:   fleet=%s user=%s org=%s\n",
		formatUSDLimit(limits.MaxMonthlyUSD), formatUSDLimit(limits.MaxMonthlyUSDPerOwner), formatUSDLimit(limits.MaxMonthlyUSDPerOrg))
}

func formatDurationSeconds(seconds int64) string {
	if seconds <= 0 {
		return "0s"
	}
	return (time.Duration(seconds) * time.Second).Round(time.Second).String()
}

func formatIntLimit(value int) string {
	if value <= 0 {
		return "off"
	}
	return fmt.Sprint(value)
}

func formatUSDLimit(value float64) string {
	if value <= 0 {
		return "off"
	}
	return fmt.Sprintf("$%.2f", value)
}
