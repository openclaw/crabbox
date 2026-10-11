//go:build !windows

package tart

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/proxmox"
)

func targetFlagFixture(t *testing.T) (core.LeaseClaim, string) {
	t.Helper()
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "CRABBOX_") {
			t.Setenv(name, "")
		}
	}
	_, _, claim := cleanupFixture(t)
	t.Chdir(t.TempDir())
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	configDir = filepath.Join(configDir, "crabbox")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "config.yaml"), []byte("provider: proxmox\ntarget: linux\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	calls := filepath.Join(bin, "calls")
	t.Setenv("CRABBOX_TART_TEST_CALLS", calls)
	const script = `#!/bin/sh
printf '%s\n' "$*" >> "$CRABBOX_TART_TEST_CALLS"
case "$1" in
list)
  if [ -d "$TART_HOME/vms/crabbox-owned-test" ]; then
    printf '[{"Name":"crabbox-owned-test","State":"stopped"}]\n'
  else
    printf '[]\n'
  fi;;
ip) printf '192.0.2.10\n';;
stop) [ "$2" = crabbox-owned-test ] || exit 92;;
delete)
  [ "$2" = crabbox-owned-test ] || exit 93
  /bin/rm -rf "$TART_HOME/vms/$2";;
*) exit 91;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "tart"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	// Only the synthetic Tart executable is reachable; no real VM or SSH access.
	t.Setenv("PATH", bin)
	return claim, calls
}

func runTargetFlagCommand(t *testing.T, args []string) error {
	t.Helper()
	var stdout, stderr bytes.Buffer
	return (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
}

func TestTartStopTargetFlagOverridesConfig(t *testing.T) {
	for _, command := range []string{"stop", "release"} {
		for _, identifier := range []string{cleanupLease, "race-slug"} {
			t.Run(command+"/"+identifier, func(t *testing.T) {
				claim, calls := targetFlagFixture(t)
				args := []string{command, "--provider", "tart", "--target", "macos", "--id", identifier}
				if err := runTargetFlagCommand(t, args); err != nil {
					t.Fatalf("explicit target must override Linux config: %v", err)
				}
				if _, exists, err := core.ReadLeaseClaimWithPresence(cleanupLease); err != nil || exists {
					t.Fatalf("released claim remains: exists=%t err=%v", exists, err)
				}
				if _, err := os.Stat(filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("released VM remains: %v", err)
				}
				data, err := os.ReadFile(calls)
				if err != nil || strings.Count(string(data), "delete "+cleanupVM+"\n") != 1 {
					t.Fatalf("expected exactly one owned VM deletion: calls=%s err=%v", data, err)
				}
			})
		}
	}
}

func TestTartStopTargetOverridePreservesSafety(t *testing.T) {
	for _, test := range []struct {
		name, target string
		loseClaim    bool
	}{
		{name: "explicit-linux", target: "linux"},
		{name: "explicit-windows", target: "windows"},
		{name: "missing-claim", target: "macos", loseClaim: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			claim, calls := targetFlagFixture(t)
			vm := filepath.Join(claim.Labels["tart_storage"], "vms", cleanupVM)
			if test.loseClaim {
				core.RemoveLeaseClaim(cleanupLease)
			}
			err := runTargetFlagCommand(t, []string{"stop", "--provider", "tart", "--target", test.target, "--id", cleanupLease})
			if err == nil {
				t.Fatal("unsafe stop accepted")
			}
			if !test.loseClaim && !strings.Contains(err.Error(), "supports target=macos only") {
				t.Fatalf("expected incompatible-target rejection: %v", err)
			}
			if !test.loseClaim {
				if err := core.VerifyLeaseClaimUnchanged(cleanupLease, claim); err != nil {
					t.Fatalf("rejected stop changed claim: %v", err)
				}
			}
			if _, err := os.Stat(vm); err != nil {
				t.Fatalf("rejected stop removed VM: %v", err)
			}
			data, readErr := os.ReadFile(calls)
			if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
				t.Fatal(readErr)
			}
			if strings.Contains(string(data), "stop ") || strings.Contains(string(data), "delete ") {
				t.Fatalf("rejected stop mutated VM: %s", data)
			}
		})
	}
}

func TestTartLifecycleTargetFlagOverridesConfig(t *testing.T) {
	for _, test := range []struct {
		command string
		extra   []string
		wantErr string
	}{
		{command: "status", extra: []string{"--id", cleanupLease}},
		{command: "inspect", extra: []string{"--id", cleanupLease}},
		{command: "list"},
		{command: "cleanup", extra: []string{"--dry-run"}},
		{command: "ssh", extra: []string{"--id", cleanupLease}, wantErr: "is stopped"},
		{command: "connect", extra: []string{"--id", cleanupLease}, wantErr: "is stopped"},
		{command: "run", extra: []string{"--id", cleanupLease, "--no-sync", "--", "true"}, wantErr: "is stopped"},
		{command: "pause", extra: []string{"--id", cleanupLease}, wantErr: "does not support pause"},
		{command: "resume", extra: []string{"--id", cleanupLease}, wantErr: "does not support resume"},
	} {
		t.Run(test.command, func(t *testing.T) {
			targetFlagFixture(t)
			args := append([]string{test.command, "--provider", "tart", "--target", "macos"}, test.extra...)
			err := runTargetFlagCommand(t, args)
			if test.wantErr == "" {
				if err != nil {
					t.Fatalf("explicit target must override Linux config: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("expected lifecycle validation %q after applying flags, got %v", test.wantErr, err)
			}
		})
	}
}
