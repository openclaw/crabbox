package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestNetworkBootstrapFallbackRestoresProviderRoute(t *testing.T) {
	for _, tc := range []struct{ name, host, public string }{
		{"public address", "203.0.113.10", "203.0.113.10"},
		{"provider proxy", "provider-route.example", "203.0.113.10"},
		{"no public address", "private-route.example", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Network = NetworkAuto
			server := Server{Labels: map[string]string{"tailscale": "true", "tailscale_fqdn": "tailnet.invalid", "tailscale_exit_node": "100.100.100.100"}}
			server.PublicNet.IPv4.IP = tc.public
			target := SSHTarget{Host: tc.host, User: "guest", Key: "lease-key", Port: "2222", FallbackPorts: []string{"22"}, ProxyCommand: "provider-proxy", NetworkKind: NetworkPublic}
			boot := bootstrapNetworkTarget(cfg, server, target)
			if boot.Host != "tailnet.invalid" || boot.NetworkKind != NetworkTailscale {
				t.Fatalf("bootstrap=%#v", boot)
			}
			got, err := resolveNetworkTarget(t.Context(), cfg, server, boot)
			if err != nil {
				t.Fatal(err)
			}
			if got.Target.Host != target.Host || got.Network != NetworkPublic || got.FallbackReason != "tailscale_unreachable" || readyNetworkDisplay(cfg, server, got.Target) != NetworkPublic {
				t.Fatalf("fallback=%#v display=%s", got, readyNetworkDisplay(cfg, server, got.Target))
			}
			if got.Target.User != target.User || got.Target.Key != target.Key || got.Target.ProxyCommand != target.ProxyCommand || got.Target.Port != target.Port || !reflect.DeepEqual(got.Target.FallbackPorts, target.FallbackPorts) {
				t.Fatalf("provider route changed: %#v", got.Target)
			}
		})
	}
}

func TestNetworkBootstrapWithoutFallbackEndpoint(t *testing.T) {
	cfg := baseConfig()
	server := Server{Labels: map[string]string{"tailscale": "true", "tailscale_fqdn": "tailnet.invalid", "tailscale_exit_node": "100.100.100.100"}}
	boot := bootstrapNetworkTarget(cfg, server, SSHTarget{Port: "22"})
	if _, err := resolveNetworkTarget(t.Context(), cfg, server, boot); err == nil || !strings.Contains(err.Error(), "no provider SSH address for fallback") {
		t.Fatalf("unusable fallback error=%v", err)
	}
}

type networkNoticeBackend struct {
	resolveResultBackend
	resolves int
	wait     bool
}

func (b *networkNoticeBackend) Resolve(ctx context.Context, req ResolveRequest) (LeaseTarget, error) {
	b.resolves++
	lease := b.lease
	lease.Server.Labels = cloneStringMap(lease.Server.Labels)
	if b.wait && b.resolves == 1 {
		lease.Server.Labels["state"] = "provisioning"
	}
	return lease, nil
}

func TestNetworkFallbackCommandNotice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("recording SSH fixture requires /bin/sh")
	}
	for _, command := range [][]string{{"ssh"}, {"inspect"}, {"inspect", "--json"}, {"status"}, {"status", "--json"}, {"status", "--wait"}, {"status", "--wait", "--json"}} {
		for _, mode := range []string{"fallback", "public", "no metadata", "tailnet", "strict"} {
			t.Run(strings.Join(command, " ")+"/"+mode, func(t *testing.T) {
				clearConfigEnv(t)
				dir := t.TempDir()
				isolateRunTestUserDirs(t, dir)
				t.Chdir(dir)
				t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
				installRecordingSSH(t, dir)
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				go func() {
					for {
						conn, err := listener.Accept()
						if err != nil {
							return
						}
						conn.Close()
					}
				}()
				_, port, _ := net.SplitHostPort(listener.Addr().String())
				server := Server{Provider: "aws", CloudID: "i-network", Labels: map[string]string{"lease": "cbx_abcdef123456", "slug": "network", "state": "ready", "tailscale": "true", "tailscale_fqdn": "tailnet.invalid"}}
				server.PublicNet.IPv4.IP = "127.0.0.1"
				network := "auto"
				switch mode {
				case "public":
					network = "public"
				case "no metadata":
					delete(server.Labels, "tailscale")
					delete(server.Labels, "tailscale_fqdn")
				case "tailnet":
					server.Labels["tailscale_fqdn"] = "127.0.0.1"
				case "strict":
					network = "tailscale"
				}
				backend := &networkNoticeBackend{resolveResultBackend: resolveResultBackend{testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}}, lease: LeaseTarget{Server: server, LeaseID: "cbx_abcdef123456", SSH: SSHTarget{Host: "127.0.0.1", User: "guest", Port: port, TargetOS: targetLinux, NoControlMaster: true}}}, wait: strings.Contains(strings.Join(command, " "), "--wait")}
				testAWSBackendOverride = backend
				t.Cleanup(func() { testAWSBackendOverride = nil })
				var stdout, stderr bytes.Buffer
				args := append(append([]string{}, command...), "--provider", "aws", "--id", "cbx_abcdef123456", "--network", network)
				if backend.wait {
					args = append(args, "--wait-timeout", "20s")
				}
				err = (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				if mode == "strict" {
					if err == nil {
						t.Fatal("strict mode succeeded")
					}
					return
				}
				if err != nil {
					t.Fatalf("err=%v stderr=%s", err, &stderr)
				}
				want := 0
				if mode == "fallback" {
					want = 1
				}
				if got := strings.Count(stderr.String(), "network fallback tailscale_unreachable"); got != want {
					t.Fatalf("notices=%d want=%d stderr=%s", got, want, &stderr)
				}
				if strings.Contains(stdout.String(), "network fallback") {
					t.Fatalf("notice in stdout: %s", &stdout)
				}
				if strings.Contains(strings.Join(command, " "), "--json") {
					var view map[string]any
					if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
						t.Fatal(err)
					}
				}
			})
		}
	}
}

func TestNetworkFallbackCoordinatorStatusNotice(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	isolateRunTestUserDirs(t, dir)
	t.Setenv("CRABBOX_NETWORK", "auto")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/leases/cbx_abcdef123456" {
			t.Errorf("unexpected coordinator request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
			ID: "cbx_abcdef123456", Provider: "aws", State: "provisioning", Host: "192.0.2.80", SSHPort: "22",
			Tailscale: &TailscaleMetadata{Enabled: true, FQDN: "tailnet.invalid"},
		}})
	}))
	defer server.Close()
	configureHeartbeatCoordinatorTest(t, server.URL)
	var stdout, stderr bytes.Buffer
	err := (App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), []string{"status", "--provider", "aws", "--id", "cbx_abcdef123456", "--json"})
	if err != nil || strings.Count(stderr.String(), "network fallback tailscale_unreachable") != 1 {
		t.Fatalf("status=%v stderr=%s", err, &stderr)
	}
	var view map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view["network"] != "public" || strings.Contains(stdout.String(), "tailscale_unreachable") {
		t.Fatalf("stdout=%s", &stdout)
	}
}
