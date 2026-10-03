package cli

import (
	"context"
	"encoding/json"
	"fmt"
)

func (a App) capacity(ctx context.Context, args []string) error {
	fs := newFlagSet("capacity", a.Stderr)
	jsonOut := fs.Bool("json", false, "print JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return Exit(2, "usage: crabbox capacity [--json]")
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
		return Exit(2, "capacity requires a configured coordinator")
	}
	res, err := coord.Capacity(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	blockedBy := ""
	if res.CoordinatorCapacityAdmission != nil && res.BlockedBy != nil {
		blockedBy = *res.BlockedBy
	}
	printDimension := func(dimension, identity string, count int, limit *int) {
		limitText := "unlimited"
		if limit != nil && *limit > 0 {
			limitText = fmt.Sprint(*limit)
		}
		blocked := ""
		if blockedBy == dimension {
			blocked = " (blocked)"
		}
		if identity != "" {
			identity = " " + identity
		}
		fmt.Fprintf(a.Stdout, "%s%s %d/%s%s\n", dimension, identity, count, limitText, blocked)
	}
	if res.CoordinatorCapacityAdmission != nil {
		printDimension("fleet", "", res.Fleet.ActiveLeases, res.Fleet.Limit)
		printDimension("org", res.Org.Key, res.Org.ActiveLeases, res.Org.Limit)
	}
	printDimension("owner", res.Owner, res.ActiveLeases, &res.EffectiveLimit)
	fmt.Fprintf(a.Stdout, "observed at: %s\n", res.ObservedAt)
	fmt.Fprintln(a.Stdout, "Snapshot only; not a reservation or approval to allocate.")
	if res.CoordinatorCapacityAdmission != nil {
		if res.Admissible {
			fmt.Fprintln(a.Stdout, "admissible: yes")
		} else {
			fmt.Fprintf(a.Stdout, "admissible: no — %s cap reached\n", blockedBy)
		}
	}
	return nil
}
