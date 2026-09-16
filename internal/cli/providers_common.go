package cli

import (
	"context"
	"errors"
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
	server.Labels = TouchDirectLeaseLabels(server.Labels, cfg, state, time.Now().UTC())
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
	if cfg.Provider == "proxmox" || server.Provider == "proxmox" {
		client, err := NewProxmoxClient(cfg)
		if err != nil {
			fmt.Fprintf(stderr, "warning: direct touch state=%s: %v\n", state, err)
			return server
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
	for attempt := 1; ; attempt++ {
		lease, err := acquire()
		if err == nil {
			return lease, nil
		}
		if !isRetryableAcquireError(err) || attempt >= acquireAttemptsForError(keep, err) {
			return LeaseTarget{}, err
		}
		if isCoordinatorStaleInstanceCleanedError(err) {
			fmt.Fprintf(rt.Stderr, "warning: coordinator returned stale instance; retrying with fresh lease: %v\n", err)
		} else {
			fmt.Fprintf(rt.Stderr, "warning: bootstrap failed; retrying with fresh lease: %v\n", err)
		}
	}
}

func acquireAttemptsForError(keep bool, err error) int {
	if isCoordinatorStaleInstanceCleanedError(err) {
		return 5
	}
	return acquireAttempts(keep)
}

func isRetryableAcquireError(err error) bool {
	return isBootstrapWaitError(err) || isCoordinatorStaleInstanceCleanedError(err)
}

type coordinatorStaleInstanceCleanedError struct {
	err error
}

func (e coordinatorStaleInstanceCleanedError) Error() string {
	return e.err.Error()
}

func (e coordinatorStaleInstanceCleanedError) Unwrap() error {
	return e.err
}

func isCoordinatorStaleInstanceCleanedError(err error) bool {
	var cleaned coordinatorStaleInstanceCleanedError
	return errors.As(err, &cleaned)
}
