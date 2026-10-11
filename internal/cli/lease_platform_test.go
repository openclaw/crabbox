package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type recordedPlatformBackend struct {
	testSSHBackend
	lease   LeaseTarget
	touches int
}

func (b *recordedPlatformBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}

func (b *recordedPlatformBackend) Touch(context.Context, TouchRequest) (Server, error) {
	b.touches++
	return b.lease.Server, nil
}

func TestLeaseCommandsRejectContradictoryPlatformFlagsBeforeGuestAccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake SSH recorder")
	}
	for _, command := range [][]string{{"status"}, {"inspect"}, {"run"}, {"ssh"}, {"connect"}, {"vnc"}, {"webvnc"}, {"code"}, {"desktop", "doctor"}} {
		t.Run(strings.Join(command, "-"), func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			isolateRunTestUserDirs(t, dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			logPath := installSSHArgsRecorder(t)
			cfg := baseConfig()
			cfg.Provider = "aws"
			backend := &recordedPlatformBackend{
				testSSHBackend: testSSHBackend{spec: (testAWSProvider{}).Spec()},
				lease: LeaseTarget{
					LeaseID: "cbx_abcdef123456",
					Server: Server{Provider: "aws", CloudID: "i-platform", Status: "active", Labels: map[string]string{
						"lease": "cbx_abcdef123456", "target": targetWindows, "windows_mode": windowsModeNormal,
					}},
					SSH: sshTargetForLease(cfg, "127.0.0.1", "", startTCPReadinessFixture(t)),
				},
			}
			testAWSBackendOverride = backend
			t.Cleanup(func() { testAWSBackendOverride = nil })
			if command[0] == "webvnc" || command[0] == "code" {
				broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/"+backend.lease.LeaseID {
						t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
						http.Error(w, "unexpected request", http.StatusBadRequest)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
						ID: backend.lease.LeaseID, Provider: "aws", State: "active", TargetOS: targetWindows, WindowsMode: windowsModeNormal,
					}})
				}))
				t.Cleanup(broker.Close)
				configureHeartbeatCoordinatorTest(t, broker.URL)
			}
			args := append(append([]string{}, command...), "--provider", "aws", "--id", backend.lease.LeaseID, "--target", "linux", "--network", "public")
			if command[0] == "run" {
				args = append(args, "--no-sync", "--", "true")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			err := (App{Stdout: io.Discard, Stderr: io.Discard}).Run(ctx, args)
			var exitErr ExitError
			if !AsExitError(err, &exitErr) || exitErr.Code != 2 || !strings.Contains(err.Error(), "lease target mismatch") {
				t.Fatalf("error=%v, want explicit lease target mismatch", err)
			}
			if data, err := os.ReadFile(logPath); err != nil && !os.IsNotExist(err) || len(data) != 0 {
				t.Fatalf("guest access before platform validation: %q err=%v", data, err)
			}
			if backend.touches != 0 {
				t.Fatalf("lease touched %d times before platform validation", backend.touches)
			}
		})
	}
}

func TestValidateResolvedLeasePlatform(t *testing.T) {
	for _, test := range []struct {
		name      string
		flags     []string
		labels    map[string]string
		wantError string
	}{
		{"defaults adopt Windows", nil, map[string]string{"target": targetWindows, "windows_mode": windowsModeWSL2}, ""},
		{"matching target", []string{"--target", "windows"}, map[string]string{"target": targetWindows, "windows_mode": windowsModeWSL2}, ""},
		{"matching mode alias", []string{"--target", "win", "--windows-mode", "native"}, map[string]string{"target": targetWindows, "windows_mode": windowsModeNormal}, ""},
		{"target mismatch", []string{"--target", "linux"}, map[string]string{"target": targetWindows}, "lease target mismatch"},
		{"mode mismatch", []string{"--target", "windows", "--windows-mode", "wsl2"}, map[string]string{"target": targetWindows, "windows_mode": windowsModeNormal}, "lease Windows mode mismatch"},
		{"mode on Linux lease", []string{"--windows-mode", "normal"}, map[string]string{"target": targetLinux}, "lease Windows mode mismatch"},
		{"legacy unknown platform", []string{"--target", "windows", "--windows-mode", "wsl2"}, nil, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := baseConfig()
			fs := newFlagSet("platform", io.Discard)
			flags := registerTargetFlags(fs, cfg)
			if err := fs.Parse(test.flags); err != nil {
				t.Fatal(err)
			}
			if err := applyTargetFlagOverrides(&cfg, fs, flags); err != nil {
				t.Fatal(err)
			}
			err := validateResolvedLeasePlatform(cfg, Server{Labels: test.labels})
			if test.wantError == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error=%v, want %q", err, test.wantError)
			}
		})
	}
}
