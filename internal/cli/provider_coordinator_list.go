package cli

import (
	"context"
	"fmt"
)

func (b *coordinatorLeaseBackend) StreamList(ctx context.Context, req ListRequest, emit func(ListPage) error) error {
	if req.All {
		return b.streamMachines(ctx, req, emit)
	}
	return b.streamUserLeases(ctx, req, emit)
}

func (b *coordinatorLeaseBackend) streamUserLeases(ctx context.Context, req ListRequest, emit func(ListPage) error) error {
	queries := []coordinatorLeaseQuery{
		{provider: b.cfg.Provider, state: "active"},
		{provider: b.cfg.Provider, state: "provisioning"},
	}
	if req.Retained {
		queries = append(queries, coordinatorLeaseQuery{provider: b.cfg.Provider, current: true})
	}
	seen := make(map[string]bool)
	truncated, err := b.coord.streamLeaseSections(ctx, queries, func(section int, leases []CoordinatorLease, more bool) error {
		page := make([]CoordinatorLease, 0, len(leases))
		for _, lease := range currentCoordinatorLeases(leases, b.cfg.Provider) {
			live := coordinatorLeaseIsLive(lease)
			if section < 2 && lease.State != queries[section].state || section == 2 && live || seen[lease.ID] {
				continue
			}
			seen[lease.ID] = true
			page = append(page, lease)
		}
		label := ""
		if section == 2 {
			label = "retained (cleanup not confirmed):"
		}
		return emit(ListPage{Servers: coordinatorLeasesToServers(page, b.cfg), JSON: page, Section: label, More: more || section+1 < len(queries)})
	})
	for _, capped := range truncated {
		if capped {
			b.warnLegacyListLimit()
			break
		}
	}
	return err
}

func coordinatorLeaseIsLive(lease CoordinatorLease) bool {
	return lease.State == "active" || lease.State == "provisioning"
}

func currentCoordinatorLeases(leases []CoordinatorLease, provider string) []CoordinatorLease {
	current := make([]CoordinatorLease, 0, len(leases))
	for _, lease := range filterCoordinatorLeasesForProvider(leases, provider) {
		retained := lease.Keep || lease.ReleaseDeletesServer != nil && !*lease.ReleaseDeletesServer
		if coordinatorLeaseIsLive(lease) || retained && !coordinatorProviderReleaseConfirmed(lease) {
			current = append(current, lease)
		}
	}
	return redactCoordinatorLeaseListSecrets(current)
}

func (b *coordinatorLeaseBackend) warnLegacyListLimit() {
	fmt.Fprintln(b.rt.Stderr, "warning: coordinator list reached its legacy 500-lease limit; older kept leases may be omitted; update the coordinator or inspect them by exact lease ID")
}

// The admin pool is a provider resource view, not a lease history view. Match
// live lease pages to that inventory as they arrive, then emit the remainder.
func (b *coordinatorLeaseBackend) streamMachines(ctx context.Context, req ListRequest, emit func(ListPage) error) error {
	coord, err := b.adminListClient()
	if err != nil {
		if err := b.allowListLeasesFallback(err); err != nil {
			return err
		}
		return b.streamUserLeases(ctx, req, emit)
	}
	ctx, cancel := context.WithCancel(ctx)
	type poolResult struct {
		machines []CoordinatorMachine
		err      error
	}
	pool := make(chan poolResult, 1)
	poolDone := make(chan struct{})
	go func() {
		defer close(poolDone)
		machines, err := coord.Pool(ctx, b.cfg)
		pool <- poolResult{machines: machines, err: err}
	}()
	defer func() { cancel(); <-poolDone }()
	var machines []CoordinatorMachine
	var poolErr, renderErr error
	poolLoaded := false
	byLease := make(map[string][]int)
	loadPool := func() error {
		if !poolLoaded {
			result := <-pool
			machines, poolErr, poolLoaded = result.machines, result.err, true
			for i, machine := range machines {
				byLease[machine.Labels["lease"]] = append(byLease[machine.Labels["lease"]], i)
			}
		}
		return poolErr
	}
	seen := make(map[int]bool)
	ids := make(map[string]struct{})
	queries := []coordinatorLeaseQuery{
		{provider: b.cfg.Provider, admin: true, state: "active"},
		{provider: b.cfg.Provider, admin: true, state: "provisioning"},
	}
	truncated, lookupErr := coord.streamLeaseSections(ctx, queries, func(section int, leases []CoordinatorLease, more bool) error {
		if err := loadPool(); err != nil {
			return err
		}
		page := make([]CoordinatorMachine, 0, len(leases))
		for _, lease := range leases {
			if lease.ID == "" || lease.State != queries[section].state {
				continue
			}
			ids[lease.ID] = struct{}{}
			for _, i := range byLease[lease.ID] {
				if !seen[i] {
					seen[i] = true
					page = append(page, machines[i])
				}
			}
		}
		renderErr = emit(ListPage{Servers: coordinatorMachinesToServers(page, ids), JSON: page, More: more || section+1 < len(queries) || len(seen) < len(machines)})
		return renderErr
	})
	if renderErr != nil {
		return renderErr
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if err := loadPool(); err != nil {
		if err := b.allowListLeasesFallback(err); err != nil {
			return err
		}
		return b.streamUserLeases(ctx, req, emit)
	}
	if lookupErr != nil {
		fmt.Fprintf(b.rt.Stderr, "warning: active lease lookup failed; orphan status unavailable: %v\n", lookupErr)
		ids = nil
	}
	for _, capped := range truncated {
		if capped {
			fmt.Fprintln(b.rt.Stderr, "warning: active lease lookup reached the legacy coordinator limit; orphan status may be incomplete")
			break
		}
	}
	remaining := make([]CoordinatorMachine, 0)
	for i, machine := range machines {
		if !seen[i] {
			remaining = append(remaining, machine)
		}
	}
	for len(remaining) > 0 {
		n := min(len(remaining), 100)
		page := remaining[:n]
		remaining = remaining[n:]
		if err := emit(ListPage{Servers: coordinatorMachinesToServers(page, ids), JSON: page, Section: "other provider inventory:", More: len(remaining) > 0}); err != nil {
			return err
		}
	}
	return nil
}
