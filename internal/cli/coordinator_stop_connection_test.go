package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCoordinatorStopUsesFreshCleanupState(t *testing.T) {
	deleting, retained := true, false
	for _, automatic := range []bool{false, true} {
		name := "explicit"
		if automatic {
			name = "automatic"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name                                        string
				state                                       string
				deletes                                     *bool
				pending, failure, retry                     string
				provider                                    string
				inspectFails, wantSSH, wantError, localOnly bool
				admin                                       bool
				network                                     NetworkMode
				tailHost, wantHost                          string
			}{
				{name: "confirmed legacy release", state: "released", localOnly: true},
				{name: "confirmed deleting release", state: "released", deletes: &deleting, localOnly: true},
				{name: "admin confirmed release", state: "released", admin: true, localOnly: true},
				{name: "admin active", state: "active", admin: true, wantSSH: true},
				{name: "active", state: "active", wantSSH: true},
				{name: "retained", state: "released", deletes: &retained, wantSSH: true},
				{name: "pending", state: "released", deletes: &deleting, pending: "2026-08-30T00:00:00Z", wantSSH: true},
				{name: "failed cleanup", state: "released", deletes: &deleting, failure: "provider error", wantSSH: true},
				{name: "scheduled cleanup", state: "released", deletes: &deleting, retry: "2026-08-30T00:00:00Z", wantSSH: true},
				{name: "inspection unavailable", inspectFails: true},
				{name: "provider changed", state: "released", provider: "external", wantError: true},
				{name: "tailscale route", state: "active", network: NetworkTailscale, tailHost: "127.0.0.1", wantHost: "127.0.0.1", wantSSH: true},
				{name: "auto tailnet route", state: "active", network: NetworkAuto, tailHost: "127.0.0.1", wantHost: "127.0.0.1", wantSSH: true},
				{name: "explicit public route", state: "active", network: NetworkPublic, tailHost: "127.0.0.1", wantSSH: true},
				{name: "confirmed tailnet deletion", state: "released", network: NetworkTailscale, tailHost: "127.0.0.1", localOnly: true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					clearConfigEnv(t)
					dir := t.TempDir()
					logPath := installRecordingSSH(t, dir)
					// The shared recorder captures command text; this test also verifies the destination.
					sshPath := filepath.Join(dir, "ssh")
					script, err := os.ReadFile(sshPath)
					if err != nil {
						t.Fatal(err)
					}
					script = []byte(strings.Replace(string(script), `cmd=""`, `printf '%s\n' "$@" >> "$CRABBOX_FAKE_SSH_LOG"
cmd=""`, 1))
					if err := os.WriteFile(sshPath, script, 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("CRABBOX_OWNER", "test@example.com")
					t.Setenv("CRABBOX_NETWORK", string(tc.network))
					port := "22"
					var tailnet *TailscaleMetadata
					if tc.tailHost != "" {
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
								_ = conn.Close()
							}
						}()
						_, port, err = net.SplitHostPort(listener.Addr().String())
						if err != nil {
							t.Fatal(err)
						}
						tailnet = &TailscaleMetadata{Enabled: true, FQDN: tc.tailHost}
					}
					const id = "cbx_abcdef123456"
					releases := 0
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						switch {
						case r.Method == http.MethodGet && r.URL.Path == "/v1/leases/"+id:
							if tc.admin && r.Header.Get("Authorization") != "Bearer admin-token" {
								http.Error(w, `{"error":"not_found"}`, http.StatusNotFound)
								return
							}
							if tc.inspectFails {
								http.Error(w, `{"error":"inspect_failed"}`, http.StatusInternalServerError)
								return
							}
							provider := tc.provider
							if provider == "" {
								provider = "aws"
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{
								ID: id, Provider: provider, State: tc.state, Host: "192.0.2.70", SSHPort: port, SSHUser: "crabbox", TargetOS: targetLinux, Tailscale: tailnet,
								ReleaseDeletesServer: tc.deletes, CleanupStartedAt: tc.pending, CleanupError: tc.failure, CleanupRetryAt: tc.retry,
							}})
						case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/"+id+"/release":
							releases++
							var body map[string]any
							if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
								t.Error(err)
							}
							if body["delete"] != true || body["expectedProvider"] != "aws" {
								t.Errorf("release body=%v", body)
							}
							_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released"}})
						default:
							http.NotFound(w, r)
						}
					}))
					defer server.Close()
					t.Setenv("CRABBOX_COORDINATOR", server.URL)
					t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
					if tc.admin {
						t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "admin-token")
					}
					var stderr bytes.Buffer
					app := App{Stdout: io.Discard, Stderr: &stderr}
					err = nil
					acquisitionLogBytes := 0
					if automatic {
						backend := coordinatorReleaseTestBackend(server, &stderr)
						backend.cfg.Network = tc.network
						if tc.admin {
							backend.cfg.Coordinator = server.URL
							backend.cfg.CoordAdminToken = "admin-token"
						}
						// Automatic cleanup retains the target acquired while the lease was active.
						acquired := LeaseTarget{
							LeaseID: id, Server: Server{Provider: "aws", Status: "active"},
							SSH: SSHTarget{Host: "192.0.2.69", Port: port, User: "crabbox", TargetOS: targetLinux},
						}
						if tailnet != nil {
							applyTailscaleMetadataToServer(&acquired.Server, *tailnet)
							resolved, err := resolveNetworkTarget(t.Context(), backend.cfg, acquired.Server, acquired.SSH)
							if err != nil {
								t.Fatal(err)
							}
							acquired.SSH = resolved.Target
							prior, err := os.ReadFile(logPath)
							if err != nil && !os.IsNotExist(err) {
								t.Fatal(err)
							}
							acquisitionLogBytes = len(prior)
						}
						err = app.releaseBackendLeaseBestEffort(context.Background(), backend, backend.cfg, acquired)
					} else {
						err = app.stop(context.Background(), []string{"--provider", "aws", "--id", id})
					}
					if (err != nil) != tc.wantError {
						t.Fatalf("stop error=%v, wantError=%v; stderr=%s", err, tc.wantError, stderr.String())
					}
					wantReleases := 1
					if tc.wantError || tc.localOnly {
						wantReleases = 0
					}
					if releases != wantReleases {
						t.Fatalf("release POSTs=%d, want %d", releases, wantReleases)
					}
					ssh, readErr := os.ReadFile(logPath)
					if readErr != nil && !os.IsNotExist(readErr) {
						t.Fatal(readErr)
					}
					ssh = ssh[acquisitionLogBytes:]
					if (len(ssh) > 0) != tc.wantSSH {
						t.Fatalf("guest SSH attempted=%v, want %v", len(ssh) > 0, tc.wantSSH)
					}
					if tc.wantSSH && !bytes.Contains(ssh, []byte(blank(tc.wantHost, "192.0.2.70"))) {
						t.Fatalf("cleanup did not use freshly resolved host: %s", ssh)
					}
					if tc.wantHost != "" && bytes.Contains(ssh, []byte("192.0.2.70")) {
						t.Fatal("cleanup replaced the selected tailnet route with the public host")
					}
					if automatic && tc.wantSSH && bytes.Contains(ssh, []byte("192.0.2.69")) {
						t.Fatal("cleanup used stale acquired host")
					}
				})
			}
		})
	}
}

func TestCoordinatorReleasePreservesCallerIdentityAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "conflicting caller lease"
		if canceled {
			name = "canceled fresh inspection"
		}
		t.Run(name, func(t *testing.T) {
			clearConfigEnv(t)
			t.Setenv("CRABBOX_OWNER", "test@example.com")
			const id = "cbx_abcdef123456"
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			releases, cleanups := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if canceled {
						cancel()
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "active"}})
				} else {
					releases++
					_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released"}})
				}
			}))
			defer server.Close()
			backend := coordinatorReleaseTestBackend(server, io.Discard)
			expected := ProviderIdentityExpectation{LeaseID: "cbx_123456abcdef"}
			if canceled {
				expected.LeaseID = id
			}
			err := backend.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}, SSH: SSHTarget{Host: "192.0.2.69"}}, ExpectedProviderIdentity: expected, GuardedRemoteCleanup: func(context.Context, LeaseTarget) { cleanups++ }})
			if err == nil || releases != 0 || cleanups != 0 {
				t.Fatalf("err=%v release POSTs=%d guest cleanups=%d; want rejection before cleanup/release", err, releases, cleanups)
			}
		})
	}
}

func TestCoordinatorStopReleasesAfterUnreachableGuestCleanup(t *testing.T) {
	if runParallelCLIContract(t, 0) {
		return
	}
	clearConfigEnv(t)
	dir := t.TempDir()
	logPath := installRecordingSSH(t, dir)
	sshPath := filepath.Join(dir, "ssh")
	script, err := os.ReadFile(sshPath)
	if err != nil {
		t.Fatal(err)
	}
	// A connected but unresponsive egress cleanup must not own the release budget.
	script = []byte(strings.Replace(string(script), `if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, `case "$match" in *"pkill -f"*) exec /bin/sleep 30 ;; esac
if [ -n "${CRABBOX_FAKE_SSH_STDIN_LOG:-}" ]; then`, 1))
	if err := os.WriteFile(sshPath, script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	const id = "cbx_abcdef123456"
	releases := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lease := CoordinatorLease{ID: id, Provider: "aws", State: "active", Host: "192.0.2.70", SSHPort: "22", SSHUser: "crabbox", TargetOS: targetLinux}
		if r.Method == http.MethodPost {
			releases++
			lease.State = "released"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "user-token")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	err = (App{Stdout: io.Discard, Stderr: io.Discard}).stop(ctx, []string{"--provider", "aws", "--id", id})
	if err != nil || releases != 1 {
		t.Fatalf("stop error=%v release POSTs=%d, want successful release after guest cleanup timeout", err, releases)
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(log, []byte("pkill -f")) {
		t.Fatal("egress cleanup was not attempted")
	}
}

func TestCoordinatorReleaseRejectsContradictoryIdentityWhenInspectFails(t *testing.T) {
	clearConfigEnv(t)
	t.Setenv("CRABBOX_OWNER", "test@example.com")
	const id = "cbx_abcdef123456"
	releases, cleanups := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, `{"error":"inspect_failed"}`, http.StatusInternalServerError)
			return
		}
		releases++
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": CoordinatorLease{ID: id, Provider: "aws", State: "released"}})
	}))
	defer server.Close()
	backend := coordinatorReleaseTestBackend(server, io.Discard)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{
		Lease:                    LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}},
		ExpectedProviderIdentity: ProviderIdentityExpectation{LeaseID: "cbx_123456abcdef"},
		GuardedRemoteCleanup:     func(context.Context, LeaseTarget) { cleanups++ },
	})
	if err == nil || releases != 0 || cleanups != 0 {
		t.Fatalf("err=%v release POSTs=%d guest cleanups=%d; want identity rejection despite failed inspection", err, releases, cleanups)
	}
}

func TestCoordinatorReleaseNetworkSelectionKeepsCleanupBounded(t *testing.T) {
	for _, mode := range []string{"selected", "unreachable", "canceled during route probe"} {
		t.Run(mode, func(t *testing.T) {
			clearConfigEnv(t)
			dir := t.TempDir()
			installRecordingSSH(t, dir)
			probeCanceled := filepath.Join(dir, "probe-canceled")
			if mode == "canceled during route probe" {
				// A connected client must not finish the SSH probe before the
				// listener cancels; TCP Accept scheduling alone cannot order that.
				sshPath := filepath.Join(dir, "ssh")
				script, err := os.ReadFile(sshPath)
				if err != nil {
					t.Fatal(err)
				}
				wait := "#!/bin/sh\nwhile [ ! -f " + shellQuote(probeCanceled) + " ]; do /bin/sleep 0.01; done\n"
				if err := os.WriteFile(sshPath, append([]byte(wait), script...), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CRABBOX_OWNER", "test@example.com")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			_, port, err := net.SplitHostPort(listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			probeDone := make(chan struct{})
			if mode == "unreachable" {
				_ = listener.Close()
				close(probeDone)
			} else {
				go func() {
					defer close(probeDone)
					conn, err := listener.Accept()
					if err == nil {
						_ = conn.Close()
						if mode == "canceled during route probe" {
							cancel()
							if err := os.WriteFile(probeCanceled, nil, 0o600); err != nil {
								t.Error(err)
							}
						}
					}
				}()
			}
			const id = "cbx_abcdef123456"
			var releases atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				lease := CoordinatorLease{ID: id, Provider: "aws", State: "active", Host: "192.0.2.70", SSHPort: port, SSHUser: "crabbox", TargetOS: targetLinux, Tailscale: &TailscaleMetadata{Enabled: true, FQDN: "127.0.0.1"}}
				if r.Method == http.MethodPost {
					releases.Add(1)
					lease.State = "released"
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"lease": lease})
			}))
			defer server.Close()
			var stderr bytes.Buffer
			backend := coordinatorReleaseTestBackend(server, &stderr)
			backend.cfg.Network = NetworkTailscale
			backend.cfg.SSHFallbackPorts = []string{}
			cleanups := 0
			err = backend.ReleaseLease(ctx, ReleaseLeaseRequest{
				Lease: LeaseTarget{LeaseID: id, Server: Server{Provider: "aws"}, SSH: SSHTarget{Host: "192.0.2.69"}},
				GuardedRemoteCleanup: func(cleanupCtx context.Context, lease LeaseTarget) {
					cleanups++
					deadline, bounded := cleanupCtx.Deadline()
					if !bounded || time.Until(deadline) > 35*time.Second || lease.SSH.Host != "127.0.0.1" || lease.SSH.Port != port {
						t.Error("cleanup lost its budget or freshly selected network route")
					}
				},
			})
			_ = listener.Close()
			<-probeDone
			wantReleases, wantCleanups := int32(1), 0
			if mode == "selected" {
				wantCleanups = 1
			}
			if mode == "canceled during route probe" {
				wantReleases = 0
			}
			if (err != nil) != (mode == "canceled during route probe") || releases.Load() != wantReleases || cleanups != wantCleanups {
				t.Fatalf("err=%v release POSTs=%d cleanups=%d; want POSTs=%d cleanups=%d", err, releases.Load(), cleanups, wantReleases, wantCleanups)
			}
			if mode == "unreachable" && !strings.Contains(stderr.String(), "could not resolve guest network before release") {
				t.Fatal("unreachable cleanup route was not reported before successful release")
			}
		})
	}
}
