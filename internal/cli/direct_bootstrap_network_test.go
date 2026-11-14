package cli

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

func TestDirectExitNodeBootstrapKeepsWaitingForSlowTailnet(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{Provider: "gcp", Network: NetworkAuto, Tailscale: TailscaleConfig{Enabled: true, ExitNode: "100.100.100.100", HostnameTemplate: "crabbox-{slug}"}}
		server := Server{Labels: map[string]string{"lease": "cbx_abcdef123456", "slug": "blue", "tailscale_hostname": "unknown"}}
		target := SSHTarget{Host: "203.0.113.10", Port: "22"}
		started := time.Now()
		wait := func(ctx context.Context, got *SSHTarget, _ time.Duration) error {
			if got.Host == "203.0.113.10" {
				<-ctx.Done()
				return ctx.Err()
			}
			if got.Host != "crabbox-blue" {
				return errors.New("wrong requested hostname")
			}
			select {
			case <-time.After(40 * time.Second):
				got.Port = "2222"
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		if err := WaitForDirectBootstrap(context.Background(), cfg, server, &target, time.Minute, wait); err != nil {
			t.Fatal(err)
		}
		if time.Since(started) != 40*time.Second {
			t.Fatalf("elapsed=%s", time.Since(started))
		}
		if target.Host != "203.0.113.10" || target.Port != "2222" {
			t.Fatalf("lost provider fallback or negotiated port: %+v", target)
		}
	})
}
