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
	"slices"
	"strings"
	"testing"
)

func TestCoordinatorResolveExplicitSSHPort(t *testing.T) {
	for _, tc := range []struct {
		name, source, port, state, provider, wantPort, wantError string
		fallback                                                 []string
		releaseOnly, statusOnly                                  bool
	}{
		{name: "implicit configuration keeps lease endpoint", port: "22", fallback: []string{"22"}, wantPort: "2222"},
		{name: "flag pins advertised fallback", source: "flag", port: "22", fallback: []string{"22"}, wantPort: "22"},
		{name: "flag pins advertised primary", source: "flag", port: "2222", fallback: []string{"22"}, wantPort: "2222"},
		{name: "environment pins advertised fallback", source: "environment", port: "22", fallback: []string{"22"}, wantPort: "22"},
		{name: "unadvertised port refuses resolution", source: "flag", port: "2200", fallback: []string{"22"}, wantError: "not advertised"},
		{name: "missing fallback does not authorize port22", source: "flag", port: "22", wantError: "not advertised"},
		{name: "provider identity remains authoritative", source: "flag", port: "22", provider: "external", fallback: []string{"22"}, wantError: "provider identity mismatch"},
		{name: "completed release does not require a guest port", source: "flag", port: "2200", state: "released"},
		{name: "release-only ignores unadvertised guest selection", source: "flag", port: "2200", fallback: []string{"22"}, releaseOnly: true, wantPort: "2222"},
		{name: "pending lease without endpoint retains custody", source: "flag", port: "22", state: "provisioning", wantPort: "22"},
		{name: "status projects explicit advertised selection", source: "environment", port: "22", state: "failed", fallback: []string{"22"}, statusOnly: true, wantPort: "22"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolateTestUserDirs(t)
			cfg := baseConfig()
			cfg.Provider, cfg.TargetOS, cfg.WindowsMode = "aws", targetWindows, windowsModeNormal
			cfg.SSHPort = tc.port
			cfg.SSHKey = filepath.Join(t.TempDir(), "client_key")
			if tc.source == "flag" {
				fs := newFlagSet("run", io.Discard)
				flags := registerLeaseCreateFlags(fs, cfg)
				if err := parseFlags(fs, []string{"--ssh-port", tc.port}); err != nil {
					t.Fatal(err)
				}
				if err := applyLeaseCreateFlagsForLease(&cfg, fs, flags, "cbx_abcdef123456"); err != nil {
					t.Fatal(err)
				}
			} else if tc.source == "environment" {
				t.Setenv("CRABBOX_SSH_PORT", tc.port)
				if err := applyEnv(&cfg); err != nil {
					t.Fatal(err)
				}
			}
			key := testOpenSSHPublicKey("ssh-ed25519", testBytes(32, 41))
			advertised := CoordinatorLease{ID: "cbx_abcdef123456", Provider: blank(tc.provider, "aws"), State: blank(tc.state, "active"), TargetOS: targetWindows, WindowsMode: windowsModeNormal, Host: "192.0.2.25", SSHUser: "lease-user", SSHPort: "2222", SSHFallbackPorts: tc.fallback, SSHHostKey: key}
			if tc.state == "provisioning" {
				advertised.Host, advertised.SSHPort, advertised.SSHHostKey = "", "", ""
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if req.Method != http.MethodGet || req.URL.Path != "/v1/leases/"+advertised.ID {
					t.Errorf("unexpected coordinator operation %s %s", req.Method, req.URL.Path)
					http.NotFound(w, req)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": advertised})
			}))
			defer server.Close()
			cfg.Coordinator, cfg.CoordToken = server.URL, "test-user-token"
			coord, _, err := newCoordinatorClient(cfg)
			if err != nil {
				t.Fatal(err)
			}
			backend := &coordinatorLeaseBackend{cfg: cfg, coord: coord}
			if tc.statusOnly {
				view, err := backend.Status(t.Context(), StatusRequest{ID: advertised.ID})
				if err != nil || view.SSHPort != tc.wantPort || len(view.SSHFallbackPorts) != 0 || view.SSHHost != advertised.Host || view.SSHUser != advertised.SSHUser || view.SSHHostKey != key || view.Ready {
					t.Fatalf("status changed endpoint authority or ignored selection: port=%s, fallback=%v, ready=%t, err=%v", view.SSHPort, view.SSHFallbackPorts, view.Ready, err)
				}
				return
			}
			lease, err := backend.Resolve(t.Context(), ResolveRequest{ID: advertised.ID, ReleaseOnly: tc.releaseOnly})
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error=%v, want %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if lease.SSH.Port != tc.wantPort {
				t.Fatalf("resolved port=%q, want %q", lease.SSH.Port, tc.wantPort)
			}
			if tc.state == "provisioning" {
				if lease.SSH.Host != "" || lease.LeaseID != advertised.ID {
					t.Fatal("pending lease lost identity or invented an endpoint")
				}
				return
			}
			if tc.state == "released" {
				if lease.SSH.Host != "" || lease.SSH.Key != "" {
					t.Fatal("released lease retained guest access")
				}
				return
			}
			if lease.LeaseID != advertised.ID || lease.Server.Provider != "aws" || lease.SSH.Host != advertised.Host || lease.SSH.User != advertised.SSHUser || lease.SSH.Key != cfg.SSHKey || lease.SSH.SSHHostKey != key {
				t.Fatal("port selection changed the lease or SSH authority")
			}
			if tc.source == "" || tc.releaseOnly {
				if !slices.Equal(lease.SSH.FallbackPorts, tc.fallback) {
					t.Fatal("implicit selection changed fallback policy")
				}
				return
			}
			if lease.SSH.FallbackPorts == nil || len(lease.SSH.FallbackPorts) != 0 {
				t.Fatalf("explicit port retained fallback routes: %v", lease.SSH.FallbackPorts)
			}
			if runtime.GOOS != "windows" {
				logDir := installWorkspaceOwnerRecordingSSH(t)
				owner, err := acquireWorkspaceOwner(t.Context(), lease.SSH, lease.LeaseID, io.Discard)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := owner.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
				args, err := os.ReadFile(filepath.Join(logDir, "1.args"))
				if err != nil || !strings.Contains(string(args), "-p\n"+tc.port+"\n") {
					t.Fatalf("owner acquisition did not use selected port: %s, error=%v", args, err)
				}
				command, _ := readWorkspaceOwnerSSHCall(t, logDir, 1)
				if command == "exit 0" {
					t.Fatal("explicit selection probed another endpoint before owner delivery")
				}
			}
		})
	}
}
