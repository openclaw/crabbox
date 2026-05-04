package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (a App) pool(ctx context.Context, args []string) error {
	if len(args) == 0 || args[0] != "list" {
		return exit(2, "usage: crabbox pool list [--json]")
	}
	return a.list(ctx, args[1:])
}

func (a App) list(ctx context.Context, args []string) error {
	fs := newFlagSet("list", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner, aws, ssh, or blacksmith-testbox")
	jsonOut := fs.Bool("json", false, "print JSON")
	targetFlags := registerTargetFlags(fs, defaultConfig())
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if isBlacksmithProvider(cfg.Provider) {
		return a.blacksmithList(ctx, cfg, *jsonOut)
	}
	if isIsloProvider(cfg.Provider) {
		return a.isloList(ctx, cfg, *jsonOut)
	}
	if isStaticProvider(cfg.Provider) {
		server, _, _, err := staticLease(cfg)
		if err != nil {
			return err
		}
		servers := []Server{server}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(servers)
		}
		for _, s := range servers {
			fmt.Fprintf(a.Stdout, "%-20s %-28s %-12s %-14s %-15s lease=%s slug=%s keep=%s target=%s\n",
				s.DisplayID(), s.Name, s.Status, s.ServerType.Name, s.PublicNet.IPv4.IP, s.Labels["lease"], blank(serverSlug(s), "-"), s.Labels["keep"], s.Labels["target"])
		}
		return nil
	}
	if _, ok, err := newCoordinatorClient(cfg); err != nil {
		return err
	} else if ok {
		if cfg.CoordAdminToken == "" {
			return exit(2, "pool list requires broker.adminToken or CRABBOX_COORDINATOR_ADMIN_TOKEN when a coordinator is configured")
		}
		cfg.CoordToken = cfg.CoordAdminToken
		coord, _, err := newCoordinatorClient(cfg)
		if err != nil {
			return err
		}
		machines, err := coord.Pool(ctx, cfg)
		if err != nil {
			return err
		}
		activeLeases, err := coord.AdminLeases(ctx, "active", "", "", 1000)
		if err != nil {
			fmt.Fprintf(a.Stderr, "warning: active lease lookup failed; orphan status unavailable: %v\n", err)
		}
		activeLeaseIDs := activeCoordinatorLeaseIDs(activeLeases)
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(machines)
		}
		for _, s := range machines {
			extra := ""
			if err == nil {
				extra = coordinatorMachineOrphanField(s.Labels, activeLeaseIDs)
			}
			fmt.Fprintf(a.Stdout, "%-20s %-28s %-12s %-14s %-15s lease=%s slug=%s keep=%s%s\n",
				s.ID, s.Name, s.Status, s.ServerType, s.Host, s.Labels["lease"], blank(s.Labels["slug"], "-"), s.Labels["keep"], extra)
		}
		return nil
	}
	if cfg.Provider == "aws" {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		servers, err := client.ListCrabboxServers(ctx)
		if err != nil {
			return err
		}
		if *jsonOut {
			return json.NewEncoder(a.Stdout).Encode(servers)
		}
		for _, s := range servers {
			fmt.Fprintf(a.Stdout, "%-20s %-28s %-12s %-14s %-15s lease=%s slug=%s keep=%s\n",
				s.DisplayID(), s.Name, s.Status, s.ServerType.Name, s.PublicNet.IPv4.IP, s.Labels["lease"], blank(serverSlug(s), "-"), s.Labels["keep"])
		}
		return nil
	}
	client, err := newHetznerClient()
	if err != nil {
		return err
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return err
	}
	if *jsonOut {
		return json.NewEncoder(a.Stdout).Encode(servers)
	}
	for _, s := range servers {
		fmt.Fprintf(a.Stdout, "%-20s %-28s %-12s %-14s %-15s lease=%s slug=%s keep=%s\n",
			s.DisplayID(), s.Name, s.Status, s.ServerType.Name, s.PublicNet.IPv4.IP, s.Labels["lease"], blank(serverSlug(s), "-"), s.Labels["keep"])
	}
	return nil
}

func activeCoordinatorLeaseIDs(leases []CoordinatorLease) map[string]struct{} {
	ids := make(map[string]struct{}, len(leases))
	for _, lease := range leases {
		if lease.ID != "" {
			ids[lease.ID] = struct{}{}
		}
	}
	return ids
}

func coordinatorMachineOrphanField(labels map[string]string, activeLeaseIDs map[string]struct{}) string {
	leaseID := labels["lease"]
	if leaseID == "" {
		return " orphan=missing-lease-label"
	}
	if _, ok := activeLeaseIDs[leaseID]; !ok {
		return " orphan=no-active-lease"
	}
	return ""
}

func (a App) machine(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return exit(2, "usage: crabbox machine cleanup [--dry-run]")
	}
	switch args[0] {
	case "cleanup":
		return a.cleanup(ctx, args[1:])
	default:
		return exit(2, "unknown machine command %q", args[0])
	}
}

func (a App) cleanup(ctx context.Context, args []string) error {
	fs := newFlagSet("machine cleanup", a.Stderr)
	provider := fs.String("provider", defaultConfig().Provider, "provider: hetzner or aws")
	dryRun := fs.Bool("dry-run", false, "only print")
	targetFlags := registerTargetFlags(fs, defaultConfig())
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	cfg.Provider = *provider
	if err := applyTargetFlagOverrides(&cfg, fs, targetFlags); err != nil {
		return err
	}
	if isStaticProvider(cfg.Provider) {
		return exit(2, "machine cleanup is not supported for provider=%s", cfg.Provider)
	}
	if _, ok, err := newCoordinatorClient(cfg); err != nil {
		return err
	} else if ok {
		return exit(2, "machine cleanup is disabled when a coordinator is configured; coordinator TTL alarms own brokered cleanup")
	}
	if cfg.Provider == "aws" {
		awsClient, err := newAWSClient(ctx, cfg)
		if err != nil {
			return err
		}
		servers, err := awsClient.ListCrabboxServers(ctx)
		if err != nil {
			return err
		}
		for _, s := range servers {
			shouldDelete, reason := shouldCleanupServer(s, time.Now().UTC())
			if !shouldDelete {
				fmt.Fprintf(a.Stderr, "skip server id=%s name=%s reason=%s\n", s.DisplayID(), s.Name, reason)
				continue
			}
			fmt.Fprintf(a.Stderr, "delete server id=%s name=%s\n", s.DisplayID(), s.Name)
			if !*dryRun {
				if err := deleteServer(ctx, cfg, s); err != nil {
					return err
				}
			}
		}
		return nil
	}
	client, err := newHetznerClient()
	if err != nil {
		return err
	}
	servers, err := client.ListCrabboxServers(ctx)
	if err != nil {
		return err
	}
	for _, s := range servers {
		shouldDelete, reason := shouldCleanupServer(s, time.Now().UTC())
		if !shouldDelete {
			fmt.Fprintf(a.Stderr, "skip server id=%s name=%s reason=%s\n", s.DisplayID(), s.Name, reason)
			continue
		}
		fmt.Fprintf(a.Stderr, "delete server id=%s name=%s\n", s.DisplayID(), s.Name)
		if !*dryRun {
			if err := deleteServer(ctx, cfg, s); err != nil {
				return err
			}
		}
	}
	return nil
}

func shouldCleanupServer(server Server, now time.Time) (bool, string) {
	labels := server.Labels
	if labels == nil {
		return false, "missing labels"
	}
	if strings.EqualFold(labels["keep"], "true") {
		return false, "keep=true"
	}
	state := strings.ToLower(labels["state"])
	switch state {
	case "running", "provisioning":
		expiresAt, ok := cleanupExpiry(labels)
		if ok && now.After(expiresAt.Add(12*time.Hour)) {
			return true, "stale state=" + state
		}
		return false, "state=" + state
	case "leased", "ready", "active":
		expiresAt, ok := cleanupExpiry(labels)
		if ok && now.After(expiresAt) {
			return true, "expired state=" + state
		}
		return false, "state=" + state
	}
	if state == "failed" || state == "released" || state == "expired" {
		return true, "state=" + state
	}
	expiresAt, ok := cleanupExpiry(labels)
	if !ok {
		return false, "missing expires_at"
	}
	if now.Before(expiresAt) {
		return false, "not expired"
	}
	return true, "expired"
}

func cleanupExpiry(labels map[string]string) (time.Time, bool) {
	for _, key := range []string{"expires_at", "ttl"} {
		value := strings.TrimSpace(labels[key])
		if value == "" {
			continue
		}
		if parsed, ok := parseLeaseLabelTime(value); ok {
			return parsed, true
		}
	}
	return time.Time{}, false
}

func directLeaseExpiresAt(now time.Time, cfg Config) time.Time {
	return directLeaseExpiresAtFrom(now, now, cfg.TTL, cfg.IdleTimeout)
}
