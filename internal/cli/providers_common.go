package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"
)

type noProviderFlags struct{}

func NoProviderFlags() any { return noProviderFlags{} }

func touchDirectLeaseBestEffort(ctx context.Context, cfg Config, server Server, state string, stderr io.Writer) Server {
	if server.Labels == nil {
		server.Labels = map[string]string{}
	}
	server.Labels = touchDirectLeaseLabels(server.Labels, cfg, state, time.Now().UTC())
	if isStaticProvider(cfg.Provider) || server.Provider == staticProvider {
		return server
	}
	if cfg.Provider == "aws" || server.Provider == "aws" || strings.HasPrefix(server.CloudID, "i-") {
		client, err := newAWSClient(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
			return server
		}
		if err := client.SetTags(ctx, server.CloudID, server.Labels); err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
		}
		return server
	}
	if cfg.Provider == "azure" || server.Provider == "azure" {
		client, err := NewAzureClient(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
			return server
		}
		name := server.CloudID
		if name == "" {
			name = server.Name
		}
		if err := client.SetTags(ctx, name, server.Labels); err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
		}
		return server
	}
	if cfg.Provider == "gcp" || server.Provider == "gcp" {
		client, err := NewGCPClient(ctx, cfg)
		if err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
			return server
		}
		if zone := server.Labels["zone"]; zone != "" {
			cfg.GCPZone = zone
			if zoned, err := NewGCPClient(ctx, cfg); err == nil {
				client = zoned
			}
		}
		if err := client.SetLabels(ctx, server.CloudID, server.Labels); err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
		}
		return server
	}
	client, err := newHetznerClient()
	if err != nil {
		fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
		return server
	}
	if err := client.SetLabels(ctx, server.ID, server.Labels); err != nil {
		fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
	}
	return server
}

func TouchDirectLeaseBestEffort(ctx context.Context, cfg Config, server Server, state string, stderr io.Writer) Server {
	return touchDirectLeaseBestEffort(ctx, cfg, server, state, stderr)
}

func acquireAttemptsRetry(rt Runtime, keep bool, acquire func() (LeaseTarget, error)) (LeaseTarget, error) {
	var lastErr error
	attempts := acquireAttempts(keep)
	for attempt := 1; attempt <= attempts; attempt++ {
		lease, err := acquire()
		if err == nil {
			return lease, nil
		}
		lastErr = err
		if attempt == attempts || !isBootstrapWaitError(err) {
			return LeaseTarget{}, err
		}
		fmt.Fprintf(rt.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
	}
	return LeaseTarget{}, lastErr
}
