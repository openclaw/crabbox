package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a App) history(ctx context.Context, args []string) error {
	fs := newFlagSet("history", a.Stderr)
	leaseID := fs.String("lease", "", "filter by lease id")
	owner := fs.String("owner", "", "filter by owner")
	org := fs.String("org", "", "filter by org")
	state := fs.String("state", "", "filter by state")
	limit := fs.Int("limit", 50, "maximum runs")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	runs, err := coord.Runs(ctx, *leaseID, *owner, *org, *state, *limit)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(runs)
	}
	for _, run := range runs {
		fmt.Fprintf(a.Stdout, "%-16s %-16s %-16s %-9s phase=%-8s exit=%s duration=%s started=%s command=%s\n",
			run.ID, run.LeaseID, blank(run.Slug, "-"), run.State, blank(run.Phase, "-"), formatRunExit(run.ExitCode), formatMs(run.DurationMs), run.StartedAt, strings.Join(run.Command, " "))
	}
	return nil
}

func (a App) logs(ctx context.Context, args []string) error {
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("logs", a.Stderr)
	runID := fs.String("id", "", "run id")
	jsonOut := fs.Bool("json", false, "print JSON metadata and log")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox logs <run-id>")
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	logText, err := coord.RunLogs(ctx, *runID)
	if err != nil {
		return err
	}
	if *jsonOut {
		run, err := coord.Run(ctx, *runID)
		if err != nil {
			return err
		}
		return json.NewEncoder(a.Stdout).Encode(map[string]any{"run": run, "log": logText})
	}
	fmt.Fprint(a.Stdout, logText)
	return nil
}

func formatRunExit(code *int) string {
	if code == nil {
		return "-"
	}
	return fmt.Sprint(*code)
}

func formatMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return (time.Duration(ms) * time.Millisecond).Round(time.Millisecond).String()
}
