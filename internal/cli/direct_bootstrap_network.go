package cli

import (
	"context"
	"errors"
	"time"
)

// WaitForDirectBootstrap gives tailnet readiness a head start for exit-node
// leases, then allows either route to finish within one shared deadline.
func WaitForDirectBootstrap(ctx context.Context, cfg Config, server Server, target *SSHTarget, timeout time.Duration, wait func(context.Context, *SSHTarget, time.Duration) error) error {
	if (cfg.Network != NetworkAuto && cfg.Network != "") || !cfg.Tailscale.Enabled || cfg.Tailscale.ExitNode == "" {
		return wait(ctx, target, timeout)
	}
	meta := serverTailscaleMetadata(server)
	// Creation labels may not contain the rendered hostname yet.
	meta.Hostname = firstNonEmpty(cfg.Tailscale.Hostname, RenderTailscaleHostname(cfg.Tailscale.HostnameTemplate, server.Labels["lease"], server.Labels["slug"], cfg.Provider))
	host := tailscaleTargetHost(meta)
	if host == "" || host == target.Host {
		return wait(ctx, target, timeout)
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	type result struct {
		target SSHTarget
		err    error
	}
	results := make(chan result, 2)
	start := func(candidate SSHTarget) {
		go func() {
			deadline, _ := deadlineCtx.Deadline()
			err := wait(deadlineCtx, &candidate, time.Until(deadline))
			results <- result{candidate, err}
		}()
	}
	candidate := *target
	candidate.Host = host
	candidate.NetworkKind = NetworkTailscale
	start(candidate)
	timer := time.NewTimer(min(30*time.Second, timeout/2))
	defer timer.Stop()
	pending := 1
	publicStarted := false
	var failures []error
	for pending > 0 {
		select {
		case ready := <-results:
			pending--
			if ready.err == nil {
				cancel()
				for pending > 0 {
					<-results
					pending--
				}
				publicHost, publicKind := target.Host, target.NetworkKind
				*target = ready.target
				// Keep the provider route available to later automatic endpoint resolution.
				if ready.target.Host == host {
					target.Host, target.NetworkKind = publicHost, publicKind
				}
				return nil
			}
			failures = append(failures, ready.err)
			if !publicStarted && deadlineCtx.Err() == nil {
				start(*target)
				pending++
				publicStarted = true
				timer.Stop()
			}
		case <-timer.C:
			if !publicStarted {
				start(*target)
				pending++
				publicStarted = true
			}
		case <-deadlineCtx.Done():
			for pending > 0 {
				<-results
				pending--
			}
			return deadlineCtx.Err()
		}
	}
	return errors.Join(failures...)
}
