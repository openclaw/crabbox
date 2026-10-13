package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
)

type CoordinatorRetainedReconcile struct {
	Version  int `json:"version"`
	Settings struct {
		Mode         string `json:"mode"`
		Delete       bool   `json:"delete"`
		Budget       int    `json:"budget"`
		GraceSeconds int    `json:"graceSeconds"`
		LookupBudget int    `json:"lookupBudget"`
	} `json:"settings"`
	Cursor     string                      `json:"cursor"`
	Cycles     int64                       `json:"cycles"`
	Scanned    int64                       `json:"scanned"`
	LastPassAt string                      `json:"lastPassAt"`
	Counters   map[string]map[string]int64 `json:"counters"`
	LastPass   *struct {
		Counters map[string]map[string]int64 `json:"counters"`
		Sample   []struct {
			ID       string `json:"id"`
			Provider string `json:"provider"`
			Outcome  string `json:"outcome"`
		} `json:"sample"`
	} `json:"lastPass,omitempty"`
	Backoff map[string]struct {
		Until    int64 `json:"until"`
		Failures int   `json:"failures"`
	} `json:"backoff"`
}

func (c *CoordinatorClient) AdminRetainedReconcile(ctx context.Context) (CoordinatorRetainedReconcile, error) {
	var result CoordinatorRetainedReconcile
	err := c.doRead(ctx, "/v1/admin/retained-reconcile", &result)
	return result, err
}

func (a App) adminRetainedReconcile(ctx context.Context, args []string) error {
	fs := newFlagSet("admin retained-reconcile", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Exit(2, "usage: crabbox admin retained-reconcile [--json]")
	}
	coord, err := configuredAdminCoordinator()
	if err != nil {
		return err
	}
	result, err := coord.AdminRetainedReconcile(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(result)
	}
	fmt.Fprintf(a.Stdout, "retained reconciliation mode=%s delete=%t budget=%d lookups=%d grace_seconds=%d scanned=%d cycles=%d last_pass=%s cursor=%s\n", result.Settings.Mode, result.Settings.Delete, result.Settings.Budget, result.Settings.LookupBudget, result.Settings.GraceSeconds, result.Scanned, result.Cycles, blank(result.LastPassAt, "-"), blank(result.Cursor, "-"))
	var rows []string
	for provider, outcomes := range result.Counters {
		for outcome, count := range outcomes {
			rows = append(rows, fmt.Sprintf("%s %s=%d", provider, outcome, count))
		}
	}
	for provider, backoff := range result.Backoff {
		rows = append(rows, fmt.Sprintf("%s backoff_until=%d failures=%d", provider, backoff.Until, backoff.Failures))
	}
	sort.Strings(rows)
	for _, row := range rows {
		fmt.Fprintln(a.Stdout, row)
	}
	return nil
}
