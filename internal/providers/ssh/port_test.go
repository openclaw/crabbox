package ssh

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestStaticClaimCannotRestoreExcludedSSHPort(t *testing.T) {
	for _, source := range []string{"static", "ssh", "implicit"} {
		t.Run(source, func(t *testing.T) {
			cfg := core.BaseConfig()
			cfg.Provider, cfg.Static.Host = "ssh", "localhost"
			want := "22"
			if source == "static" {
				cfg.Static.Port, want = "2224", "2224"
			}
			if source == "ssh" {
				cfg.SSHPort, want = "2224", "2224"
				core.MarkSSHPortExplicit(&cfg)
			}
			_, initial, _, err := core.StaticLease(cfg)
			if err != nil {
				t.Fatal(err)
			}
			claim := core.LeaseClaim{LeaseID: "static_port_test", SSHPort: 22, Labels: map[string]string{"architecture_route": architectureEndpoint(initial)}}
			_, target, _, err := staticLeaseFromClaim(cfg, claim)
			if err != nil {
				t.Fatal(err)
			}
			if target.Port != want {
				t.Fatalf("stored fallback replaced selected port: got %s want %s", target.Port, want)
			}
		})
	}
}

func TestStaticRunExplicitPortAtReadiness(t *testing.T) {
	for _, source := range []string{"static-port", "ssh-port"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("CRABBOX_CONFIG", filepath.Join(t.TempDir(), "absent.yaml"))
			old := waitForSSH
			t.Cleanup(func() { waitForSSH = old })
			sentinel := errors.New("stopped before remote effects")
			called := false
			waitForSSH = func(_ context.Context, target *core.SSHTarget, _ io.Writer) error {
				called = true
				if target.Host != "127.0.0.1" || target.Port != "2224" || target.FallbackPorts == nil || len(target.FallbackPorts) != 0 {
					t.Fatalf("run did not pin explicit endpoint: host=%s port=%s fallback=%v", target.Host, target.Port, target.FallbackPorts)
				}
				return sentinel
			}
			var log bytes.Buffer
			err := (core.App{Stdout: &log, Stderr: &log}).Run(t.Context(), []string{"run", "--provider", "ssh", "--static-host", "127.0.0.1", "--" + source, "2224", "--no-sync", "--", "true"})
			if !called || !errors.Is(err, sentinel) {
				t.Fatalf("run did not reach real static backend: %v\n%s", err, &log)
			}
		})
	}
}
