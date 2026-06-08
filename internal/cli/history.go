package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		telemetry := runTelemetryStatusSummary(run.Telemetry)
		if telemetry != "" {
			telemetry = " resources=" + telemetry
		}
		fmt.Fprintf(a.Stdout, "%-16s %-16s %-16s %-9s phase=%s exit=%s duration=%s started=%s%s command=%s\n",
			run.ID, blank(run.LeaseID, "-"), blank(run.Slug, "-"), run.State, blank(run.Phase, "-"), formatRunExit(run.ExitCode), formatMs(run.DurationMs), run.StartedAt, telemetry, strings.Join(run.Command, " "))
	}
	return nil
}

func (a App) logs(ctx context.Context, args []string) error {
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("logs", a.Stderr)
	runIDValue, args := popLeadingRunID(args)
	runID := fs.String("id", runIDValue, "run id")
	tail := fs.Int("tail", 0, "print only the last N log lines")
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
	if *tail < 0 {
		return exit(2, "tail must be >= 0")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	logText, err := coord.RunLogs(ctx, *runID)
	if err != nil {
		return err
	}
	if *tail > 0 {
		logText = tailLogLines(logText, *tail)
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

func (a App) events(ctx context.Context, args []string) error {
	args, jsonAnywhere := extractBoolFlag(args, "json")
	fs := newFlagSet("events", a.Stderr)
	runIDValue, args := popLeadingRunID(args)
	runID := fs.String("id", runIDValue, "run id")
	after := fs.Int("after", 0, "only show events after this sequence")
	limit := fs.Int("limit", 500, "maximum events")
	eventType := fs.String("type", "", "only show events with this type")
	phase := fs.String("phase", "", "only show events with this phase")
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox events <run-id>")
	}
	if jsonAnywhere {
		*jsonOut = true
	}
	if *after < 0 {
		return exit(2, "after must be >= 0")
	}
	if *limit <= 0 {
		return exit(2, "limit must be positive")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	eventTypeFilter := strings.TrimSpace(*eventType)
	phaseFilter := strings.TrimSpace(*phase)
	events, err := fetchFilteredRunEvents(ctx, coord, *runID, *after, *limit, eventTypeFilter, phaseFilter)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(events)
	}
	for _, event := range events {
		text := event.Message
		if text == "" {
			text = strings.TrimSpace(event.Data)
		}
		fmt.Fprintf(a.Stdout, "%04d %-18s phase=%s stream=%s at=%s %s\n",
			event.Seq, event.Type, blank(event.Phase, "-"), blank(event.Stream, "-"), event.CreatedAt, text)
	}
	return nil
}

func (a App) attach(ctx context.Context, args []string) error {
	fs := newFlagSet("attach", a.Stderr)
	runIDValue, args := popLeadingRunID(args)
	runID := fs.String("id", runIDValue, "run id")
	after := fs.Int("after", 0, "resume after this event sequence")
	poll := fs.Duration("poll", time.Second, "poll interval")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if *runID == "" && fs.NArg() > 0 {
		*runID = fs.Arg(0)
	}
	if *runID == "" {
		return exit(2, "usage: crabbox attach <run-id>")
	}
	if *after < 0 {
		return exit(2, "after must be >= 0")
	}
	if *poll <= 0 {
		return exit(2, "poll must be positive")
	}
	coord, err := configuredCoordinator()
	if err != nil {
		return err
	}
	nextAfter := *after
	if wsAfter, done, used, err := followRunControlWebSocket(ctx, coord, *runID, nextAfter, *poll, a.Stdout, a.Stderr); err != nil {
		return err
	} else if done {
		return nil
	} else if used {
		nextAfter = wsAfter
	}
	for {
		events, err := coord.RunEvents(ctx, *runID, nextAfter, 100)
		if err != nil {
			return err
		}
		for _, event := range events {
			if event.Seq > nextAfter {
				nextAfter = event.Seq
			}
			printAttachEvent(a.Stdout, a.Stderr, event)
		}
		if len(events) == 0 {
			run, err := coord.Run(ctx, *runID)
			if err != nil {
				return err
			}
			if run.State != "running" {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(*poll):
		}
	}
}

func printAttachEvent(stdout, stderr io.Writer, event CoordinatorRunEvent) {
	stream := event.Stream
	if stream == "" && (event.Type == "stdout" || event.Type == "stderr") {
		stream = event.Type
	}
	switch stream {
	case "stdout":
		fmt.Fprint(stdout, outputEventText(event))
	case "stderr":
		fmt.Fprint(stderr, outputEventText(event))
	default:
		text := event.Message
		if text == "" {
			text = strings.TrimSpace(event.Data)
		}
		fmt.Fprintf(stderr, "%04d %-18s phase=%s at=%s %s\n",
			event.Seq, event.Type, blank(event.Phase, "-"), event.CreatedAt, text)
	}
}

func outputEventText(event CoordinatorRunEvent) string {
	if event.Data != "" {
		return event.Data
	}
	return event.Message
}

func popLeadingRunID(args []string) (string, []string) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return "", args
	}
	return args[0], args[1:]
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

func tailLogLines(text string, n int) string {
	if n <= 0 || text == "" {
		return text
	}
	lines := strings.SplitAfter(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[len(lines)-n:], "")
}

func fetchFilteredRunEvents(ctx context.Context, coord *CoordinatorClient, runID string, after, limit int, eventType, phase string) ([]CoordinatorRunEvent, error) {
	if eventType == "" && phase == "" {
		return coord.RunEvents(ctx, runID, after, limit)
	}
	const pageSize = 500
	var filtered []CoordinatorRunEvent
	nextAfter := after
	for len(filtered) < limit {
		page, err := coord.RunEvents(ctx, runID, nextAfter, pageSize)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		for _, event := range page {
			if event.Seq > nextAfter {
				nextAfter = event.Seq
			}
			if eventType != "" && event.Type != eventType {
				continue
			}
			if phase != "" && event.Phase != phase {
				continue
			}
			filtered = append(filtered, event)
			if len(filtered) >= limit {
				break
			}
		}
		if len(page) < pageSize {
			break
		}
	}
	if filtered == nil {
		return []CoordinatorRunEvent{}, nil
	}
	return filtered, nil
}
