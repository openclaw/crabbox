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
		return exit(2, "usage: crabbox capacity [--json]")
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
		return exit(2, "capacity requires a configured coordinator")
	}
	res, err := coord.Capacity(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(res)
	}
	fmt.Fprintf(a.Stdout, "self-owner admission count: owner=%s activeLeases=%d\n", res.Owner, res.ActiveLeases)
	fmt.Fprintf(a.Stdout, "effective owner limit: %s\nobserved at: %s\n", formatIntLimit(res.EffectiveLimit), res.ObservedAt)
	fmt.Fprintln(a.Stdout, "Snapshot only; not a reservation or approval to allocate.")
	return nil
}
