package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestCoordinatorFreshWindowsBootstrapTargets(t *testing.T) {
	for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
		for _, tc := range []struct {
			name, port, provider, wantError string
			advertised                      []string
			wantInitial                     []string
		}{
			{name: "config pins 2222", port: "2222", advertised: []string{"22"}, wantInitial: []string{"2222", "22"}},
			{name: "config pins advertised 22", port: "22", advertised: []string{"22"}, wantInitial: []string{"22"}},
			{name: "only initial foothold bypasses pin", port: "2222", advertised: []string{"22", "2200"}, wantInitial: []string{"2222", "22"}},
			{name: "implicit default", advertised: []string{"22"}, wantInitial: []string{"2222", "22"}},
			{name: "omitted advertisement does not invent 22", port: "2222", wantInitial: []string{"2222"}},
			{name: "empty advertisement does not invent 22", port: "2222", advertised: []string{}, wantInitial: []string{"2222"}},
			{name: "unadvertised explicit 22 rejected", port: "22", wantError: "not advertised"},
			{name: "unadvertised workload port rejected", port: "2200", advertised: []string{"22"}, wantError: "not advertised"},
			{name: "provider identity rejected", port: "2222", provider: "azure", advertised: []string{"22"}, wantError: "provider identity mismatch"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				cfg, lease := freshWindowsBootstrapFixture(t, mode, tc.port)
				lease.SSHFallbackPorts = tc.advertised
				if tc.provider != "" {
					lease.Provider = tc.provider
				}
				before := lease
				before.SSHFallbackPorts = slices.Clone(lease.SSHFallbackPorts)
				backend := &coordinatorLeaseBackend{cfg: cfg}
				workload, initial, err := backend.prepareCoordinatorLeaseAcquisition(lease, cfg)
				if tc.wantError != "" {
					if err == nil || !strings.Contains(err.Error(), tc.wantError) {
						t.Fatalf("error=%v, want %q", err, tc.wantError)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(lease, before) {
					t.Fatal("preparation changed the coordinator advertisement")
				}
				if got := sshPortCandidates(initial.Port, initial.FallbackPorts); !slices.Equal(got, tc.wantInitial) {
					t.Fatalf("initial bootstrap ports=%v, want %v", got, tc.wantInitial)
				}
				if workload.SSH.Port != blank(tc.port, "2222") || workload.SSH.WindowsMode != mode {
					t.Fatalf("workload selection changed: %+v", workload.SSH)
				}
				if tc.port != "" && (workload.SSH.FallbackPorts == nil || len(workload.SSH.FallbackPorts) != 0) {
					t.Fatalf("explicit workload retained fallbacks: %v", workload.SSH.FallbackPorts)
				}
				if tc.port == "" && !slices.Equal(workload.SSH.FallbackPorts, tc.advertised) {
					t.Fatal("implicit workload lost fallback policy")
				}
				if workload.SSH.User != lease.SSHUser || initial.User != "Administrator" || initial.WindowsMode != windowsModeNormal {
					t.Fatal("Windows bootstrap changed the existing user or shell contract")
				}
				if workload.Server.Provider != "aws" || workload.LeaseID != lease.ID {
					t.Fatal("preparation lost provider or lease identity")
				}
				for _, target := range []SSHTarget{workload.SSH, initial} {
					if target.Host != lease.Host || target.Key != cfg.SSHKey || target.SSHHostKey != lease.SSHHostKey || target.KnownHostsFile != workload.SSH.KnownHostsFile || target.HostKeyAlias != workload.SSH.HostKeyAlias {
						t.Fatal("bootstrap route changed host, key, or host-key trust")
					}
					assertSSHOption(t, sshBaseArgs(target), "StrictHostKeyChecking", "yes")
				}
				pin, err := os.ReadFile(initial.KnownHostsFile)
				if err != nil || string(pin) != initial.HostKeyAlias+" "+sshKeyWithoutComment(lease.SSHHostKey)+"\n" {
					t.Fatalf("bootstrap lost authoritative host-key pin: %q, %v", pin, err)
				}
				// Resolution for subsequent commands still uses the strict selector.
				reused, err := backend.coordinatorLeaseTargetForConfig(lease, cfg, nil)
				if err != nil || reused.SSH.Port != workload.SSH.Port || !slices.Equal(reused.SSH.FallbackPorts, workload.SSH.FallbackPorts) || reused.SSH.User != lease.SSHUser {
					t.Fatalf("reuse changed its port or user contract: %+v, %v", reused.SSH, err)
				}
			})
		}
	}
}

func TestCoordinatorFreshWindowsBootstrapPreservesOtherProviders(t *testing.T) {
	for _, provider := range []string{"azure", "ssh"} {
		for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
			t.Run(provider+"/"+mode, func(t *testing.T) {
				cfg, lease := freshWindowsBootstrapFixture(t, mode, "2222")
				cfg.Provider, lease.Provider = provider, provider
				backend := &coordinatorLeaseBackend{cfg: cfg}
				workload, initial, err := backend.prepareCoordinatorLeaseAcquisition(lease, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if initial.Port != "2222" || len(initial.FallbackPorts) != 0 || initial.User != lease.SSHUser || workload.SSH.User != lease.SSHUser {
					t.Fatal("AWS bootstrap route escaped into another provider")
				}
			})
		}
	}
}

func TestCoordinatorFreshWindowsBootstrapDelivery(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake SSH executable requires a POSIX shell")
	}
	for _, mode := range []string{windowsModeNormal, windowsModeWSL2} {
		t.Run(mode, func(t *testing.T) {
			cfg, lease := freshWindowsBootstrapFixture(t, mode, "2222")
			backend := &coordinatorLeaseBackend{cfg: cfg}
			workload, initial, err := backend.prepareCoordinatorLeaseAcquisition(lease, cfg)
			if err != nil {
				t.Fatal(err)
			}
			logDir := installWindowsBootstrapSSH(t)
			// A synthetic proxy routes all SSH to the fake executable; no guest
			// sockets, provider credentials, or privileged local ports are needed.
			initial.SSHConfigProxy, workload.SSH.SSHConfigProxy = true, true
			initial.NoControlMaster, workload.SSH.NoControlMaster = true, true
			ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
			defer cancel()
			if err := bootstrapPreparedManagedWindowsDesktop(ctx, cfg, &workload.SSH, initial, "ssh-ed25519 fixture", io.Discard); err != nil {
				t.Fatal(err)
			}
			if workload.SSH.Port != "2222" || len(workload.SSH.FallbackPorts) != 0 || workload.SSH.WindowsMode != mode {
				t.Fatalf("initial port leaked into final workload: %+v", workload.SSH)
			}
			calls, err := os.ReadFile(filepath.Join(logDir, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(calls), "initial 22\n") || !strings.Contains(string(calls), "ready 2222\n") || strings.Contains(string(calls), "ready 22\n") {
				t.Fatalf("bootstrap did not transition from 22 to pinned 2222:\n%s", calls)
			}
			payload, err := os.ReadFile(filepath.Join(logDir, "bootstrap.ps1"))
			if err != nil || string(payload) != windowsBootstrapPowerShell(cfg, "ssh-ed25519 fixture") {
				t.Fatalf("bootstrap did not receive the configured final-port script: %v", err)
			}
			if mode == windowsModeNormal {
				t.Setenv("CRABBOX_TEST_WINDOWS_WORKLOAD_FAIL", "1")
				err := runSSHInput(ctx, workload.SSH, powershellCommand("exit 73"), nil, io.Discard, io.Discard)
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != 73 {
					t.Fatalf("fake workload failure was lost: %v", err)
				}
				after, readErr := os.ReadFile(filepath.Join(logDir, "calls"))
				if readErr != nil || string(after[len(calls):]) != "ready 2222\n" {
					t.Fatalf("failed workload replayed or tried another port: %s, %v", after, readErr)
				}
			}
		})
	}
}

func freshWindowsBootstrapFixture(t *testing.T, mode, explicitPort string) (Config, CoordinatorLease) {
	t.Helper()
	isolateTestUserDirs(t)
	cfg := baseConfig()
	cfg.Provider, cfg.TargetOS, cfg.WindowsMode = "aws", targetWindows, mode
	cfg.SSHPort, cfg.SSHFallbackPorts = "2222", []string{"22"}
	cfg.SSHKey = filepath.Join(t.TempDir(), "client_key")
	if explicitPort != "" {
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte("ssh:\n  port: \""+explicitPort+"\"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		file, err := readFileConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := applyFileConfig(&cfg, file); err != nil {
			t.Fatal(err)
		}
		if !IsSSHPortExplicit(&cfg) {
			t.Fatal("config-file SSH port did not retain explicit provenance")
		}
	}
	return cfg, CoordinatorLease{
		ID: "cbx_abcdef123456", Provider: "aws", State: "active", TargetOS: targetWindows, WindowsMode: mode,
		Host: "127.0.0.1", SSHUser: "lease-user", SSHPort: "2222", SSHFallbackPorts: []string{"22"},
		SSHHostKey: testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 41)),
	}
}

func installWindowsBootstrapSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
port=
while [ "$#" -gt 0 ]; do
  if [ "$1" = -p ]; then shift; port=$1; fi
  shift
done
phase=initial
if [ -f "$CRABBOX_TEST_WINDOWS_SSH/ready" ]; then phase=ready; fi
printf '%s %s\n' "$phase" "$port" >> "$CRABBOX_TEST_WINDOWS_SSH/calls"
if [ "$phase" = initial ] && [ "$port" != 22 ]; then exit 255; fi
if [ "${CRABBOX_TEST_WINDOWS_WORKLOAD_FAIL:-}" = 1 ]; then exit 73; fi
cat > "$CRABBOX_TEST_WINDOWS_SSH/input"
if [ -s "$CRABBOX_TEST_WINDOWS_SSH/input" ]; then
  cp "$CRABBOX_TEST_WINDOWS_SSH/input" "$CRABBOX_TEST_WINDOWS_SSH/bootstrap.ps1"
  touch "$CRABBOX_TEST_WINDOWS_SSH/ready"
fi
exit 0
`
	if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("CRABBOX_TEST_WINDOWS_SSH", dir)
	t.Setenv("CRABBOX_TEST_WINDOWS_WORKLOAD_FAIL", "")
	return dir
}
