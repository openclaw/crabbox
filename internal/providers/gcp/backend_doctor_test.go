package gcp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
	"google.golang.org/api/googleapi"
)

type fakeGCPDoctorClient struct {
	listCalls      int
	deleted        []string
	mutated        bool
	servers        []core.Server
	complete       []core.Server
	get            map[string]core.Server
	getErr         error
	observe        func(context.Context, string) (core.Server, error)
	created        core.Server
	createCfg      core.Config
	createErr      error
	deleteErr      error
	deleteObserve  func(context.Context, string) error
	createCalls    int
	createLeaseIDs []string
	labeledNames   []string
	labeledValues  []map[string]string
}

func (c *fakeGCPDoctorClient) ListCrabboxServers(context.Context) ([]core.Server, error) {
	c.listCalls++
	return c.servers, nil
}

func (c *fakeGCPDoctorClient) ListCrabboxServersComplete(context.Context) ([]core.Server, error) {
	c.listCalls++
	if c.complete != nil {
		return c.complete, nil
	}
	return c.servers, nil
}

func (c *fakeGCPDoctorClient) CreateServerWithFallback(_ context.Context, _ core.Config, _ string, leaseID, _ string, _ bool, _ func(string, ...any)) (core.Server, core.Config, error) {
	c.createCalls++
	c.createLeaseIDs = append(c.createLeaseIDs, leaseID)
	c.mutated = true
	if c.createErr != nil {
		return core.Server{}, core.Config{}, c.createErr
	}
	if c.created.CloudID == "" {
		c.created = core.Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{}}
	}
	return c.created, c.createCfg, nil
}

func (c *fakeGCPDoctorClient) GetServer(ctx context.Context, name string) (core.Server, error) {
	if c.observe != nil {
		return c.observe(ctx, name)
	}
	if c.getErr != nil {
		return core.Server{}, c.getErr
	}
	if c.get != nil {
		if server, ok := c.get[name]; ok {
			return server, nil
		}
	}
	return core.Server{}, errors.New("gcp server not found: " + name)
}

func (c *fakeGCPDoctorClient) DeleteServer(ctx context.Context, name string) error {
	c.deleted = append(c.deleted, name)
	c.mutated = true
	if c.deleteObserve != nil {
		return c.deleteObserve(ctx, name)
	}
	return c.deleteErr
}

func (c *fakeGCPDoctorClient) SetLabels(_ context.Context, name string, labels map[string]string) error {
	c.mutated = true
	c.labeledNames = append(c.labeledNames, name)
	c.labeledValues = append(c.labeledValues, maps.Clone(labels))
	return nil
}

func TestGCPTouchPreservesZoneAndIdlePolicy(t *testing.T) {
	override := 90 * time.Minute
	for _, tc := range []struct {
		name      string
		storedKey string
		override  *time.Duration
		want      string
	}{
		{"preserve", "idle_timeout_secs", nil, "1800"},
		{"replace", "idle_timeout_secs", &override, "5400"},
		{"preserve legacy", "idle_timeout", nil, "1800"},
		{"replace legacy", "idle_timeout", &override, "5400"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeGCPDoctorClient{}
			var zones []string
			old := newGCPClient
			newGCPClient = func(_ context.Context, cfg core.Config) (gcpClient, error) {
				zones = append(zones, cfg.GCP.Zone)
				return fake, nil
			}
			t.Cleanup(func() { newGCPClient = old })
			server := canonicalGCPTestServer("cbx_123456abcdef", "idle-policy")
			server.Labels[tc.storedKey] = "1800"
			before := maps.Clone(server.Labels)
			cfg := core.Config{GCP: core.GCPConfig{Zone: "us-central1-a"}, IdleTimeout: 5 * time.Minute}
			backend := NewGCPLeaseBackend(Provider{}.Spec(), cfg, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
			got, err := backend.Touch(context.Background(), core.TouchRequest{Lease: core.LeaseTarget{Server: server}, State: "ready", IdleTimeout: cfg.IdleTimeout, IdleTimeoutOverride: tc.override})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(zones, []string{"us-central1-a", "us-central1-b"}) || !reflect.DeepEqual(fake.labeledNames, []string{server.CloudID}) {
				t.Fatalf("zones=%v names=%v", zones, fake.labeledNames)
			}
			if !maps.Equal(fake.labeledValues[0], got.Labels) || got.Labels["idle_timeout_secs"] != tc.want || got.Labels["idle_timeout"] != tc.want {
				t.Fatalf("written=%v returned=%v want timeout=%s", fake.labeledValues, got.Labels, tc.want)
			}
			if !maps.Equal(server.Labels, before) || got.Labels["zone"] != before["zone"] {
				t.Fatal("touch changed input labels or the provider zone")
			}
		})
	}
}

func canonicalGCPTestServer(leaseID, slug string) core.Server {
	name := core.LeaseProviderName(leaseID, slug)
	return core.Server{
		Provider: "gcp",
		CloudID:  name,
		ID:       42,
		Name:     name,
		Labels: map[string]string{
			"crabbox":      "true",
			"created_by":   "crabbox",
			"provider":     "gcp",
			"provider_key": "crabbox-test",
			"lease":        leaseID,
			"slug":         slug,
			"zone":         "us-central1-b",
		},
	}
}

func claimGCPTestServer(t *testing.T, cfg core.Config, server core.Server) {
	t.Helper()
	if err := core.ClaimLeaseTargetForConfig(server.Labels["lease"], server.Labels["slug"], cfg, server, core.SSHTarget{}, time.Minute); err != nil {
		t.Fatal(err)
	}
}

func gcpTestConnectionArtifacts(t *testing.T, leaseID string) string {
	t.Helper()
	key, err := core.PrepareStoredTestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(key)
	for _, name := range []string{"id_ed25519", "id_ed25519.pub", "known_hosts"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("synthetic fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestValidateExactGCPClaimBindsProviderResourceAndLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}
	server := canonicalGCPTestServer("cbx_111111111111", "owned")
	claimGCPTestServer(t, cfg, server)
	claim, err := core.ReadLeaseClaim(server.Labels["lease"])
	if err != nil {
		t.Fatal(err)
	}
	if err := validateExactGCPClaim(claim, server, server.Labels["lease"], cfg); err != nil {
		t.Fatalf("valid exact claim rejected: %v", err)
	}

	tests := map[string]func(*core.LeaseClaim, *core.Server, *core.Config){
		"project scope": func(_ *core.LeaseClaim, _ *core.Server, cfg *core.Config) { cfg.GCP.Project = "project-b" },
		"cloud name":    func(_ *core.LeaseClaim, server *core.Server, _ *core.Config) { server.CloudID += "-other" },
		"numeric id":    func(_ *core.LeaseClaim, server *core.Server, _ *core.Config) { server.ID++ },
		"slug": func(_ *core.LeaseClaim, server *core.Server, _ *core.Config) {
			server.Labels["slug"] = "other"
			server.Name = core.LeaseProviderName(server.Labels["lease"], "other")
		},
		"zone": func(_ *core.LeaseClaim, server *core.Server, _ *core.Config) {
			server.Labels["zone"] = "us-central1-c"
		},
		"provider key": func(_ *core.LeaseClaim, server *core.Server, _ *core.Config) {
			server.Labels["provider_key"] = "crabbox-other"
		},
		"claim labels": func(claim *core.LeaseClaim, _ *core.Server, _ *core.Config) { delete(claim.Labels, "zone") },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			changedClaim := claim
			changedClaim.Labels = maps.Clone(claim.Labels)
			changedServer := server
			changedServer.Labels = maps.Clone(server.Labels)
			changedCfg := cfg
			mutate(&changedClaim, &changedServer, &changedCfg)
			if err := validateExactGCPClaim(changedClaim, changedServer, server.Labels["lease"], changedCfg); err == nil {
				t.Fatal("mismatched claim was accepted")
			}
		})
	}
}

func TestGCPReleaseLeaseRejectsMissingExactClaimBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	server := canonicalGCPTestServer("cbx_222222222222", "claimless")
	fake := &fakeGCPDoctorClient{get: map[string]core.Server{server.CloudID: server}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: server.Labels["lease"], Server: server}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("ReleaseLease() error=%v, want missing-claim refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v, want no destructive call", fake.deleted)
	}
}

func TestValidateGCPCleanupLiveServerRejectsReplacementInstance(t *testing.T) {
	expected := canonicalGCPTestServer("cbx_111111111111", "stale")
	live := expected
	live.ID++
	if err := validateGCPCleanupLiveServer(expected, live); err == nil || !strings.Contains(err.Error(), "instance id") {
		t.Fatalf("replacement instance error=%v", err)
	}
}

func TestWaitForServerIPReadinessBudget(t *testing.T) {
	const name = "readiness-instance"
	ready := core.Server{CloudID: name, Labels: map[string]string{"state": "provisioning"}}
	ready.PublicNet.IPv4.IP = "192.0.2.10"
	readErr := errors.New("instance observation unavailable")
	callerCause := errors.New("caller stopped GCP readiness")
	for _, tc := range []struct {
		name         string
		wantCalls    int
		wantMaxCalls int
		wantElapsed  time.Duration
		wantErr      error
		wantTimeout  bool
	}{
		{name: "ready", wantCalls: 1},
		{name: "pending then ready", wantCalls: 2, wantElapsed: 5 * time.Second},
		{name: "observation error", wantCalls: 1, wantErr: readErr},
		{name: "client deadline", wantCalls: 1, wantErr: context.DeadlineExceeded},
		{name: "pre-canceled", wantErr: callerCause},
		{name: "cancel during wait", wantCalls: 1, wantElapsed: time.Second, wantErr: callerCause},
		{name: "caller deadline", wantCalls: 1, wantElapsed: time.Second, wantErr: context.DeadlineExceeded},
		{name: "blocked observation", wantCalls: 1, wantElapsed: 2 * time.Minute, wantTimeout: true},
		// The final sleep and deadline may wake together before cancellation is delivered.
		{name: "pending timeout", wantCalls: 24, wantMaxCalls: 25, wantElapsed: 2 * time.Minute, wantTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				if tc.name == "pre-canceled" {
					cancel(callerCause)
				}
				guard := 3 * time.Minute
				if tc.name == "caller deadline" {
					guard = time.Second
				}
				ctx, stop := context.WithTimeout(parent, guard)
				defer stop()
				if tc.name == "cancel during wait" {
					time.AfterFunc(time.Second, func() { cancel(callerCause) })
				}
				calls := 0
				started := time.Now()
				client := &fakeGCPDoctorClient{observe: func(observeCtx context.Context, gotName string) (core.Server, error) {
					calls++
					if gotName != name {
						t.Fatalf("observed %q, want %q", gotName, name)
					}
					switch tc.name {
					case "pending then ready":
						if calls == 1 {
							return core.Server{CloudID: name}, nil
						}
					case "observation error", "client deadline":
						return ready, tc.wantErr
					case "cancel during wait", "pending timeout":
						return core.Server{CloudID: name}, nil
					case "blocked observation", "caller deadline":
						if tc.name == "blocked observation" {
							deadline, bounded := observeCtx.Deadline()
							if !bounded || !deadline.Equal(started.Add(2*time.Minute)) {
								t.Errorf("observation deadline=%v bounded=%t, want two-minute readiness budget", deadline, bounded)
							}
						}
						<-observeCtx.Done()
						return core.Server{}, observeCtx.Err()
					}
					return ready, nil
				}}
				server, err := waitForServerIP(ctx, client, name)
				if tc.wantTimeout || tc.wantErr != nil {
					if err == nil || !reflect.DeepEqual(server, core.Server{}) {
						t.Fatalf("server=%+v err=%v, want zero server on failure", server, err)
					}
					if tc.wantTimeout && err.Error() != "timeout waiting for gcp public ip on "+name {
						t.Fatalf("timeout diagnostic changed: %v", err)
					}
					if tc.wantTimeout && (!errors.Is(err, context.DeadlineExceeded) || core.ExitCodeForError(err, 1) != 1) {
						t.Fatalf("timeout identity/code lost: err=%v code=%d", err, core.ExitCodeForError(err, 1))
					}
					if tc.wantTimeout {
						got := core.FinalizeRunResult(core.RunResult{}, err)
						want := core.FinalizeRunResult(core.RunResult{}, context.DeadlineExceeded)
						if got.Status != want.Status || got.ErrorKind != want.ErrorKind {
							t.Fatalf("timeout classification=%s/%s want=%s/%s", got.Status, got.ErrorKind, want.Status, want.ErrorKind)
						}
					}
					if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
						t.Fatalf("err=%v, want %v", err, tc.wantErr)
					}
				} else if err != nil || !reflect.DeepEqual(server, ready) {
					t.Fatalf("server=%+v err=%v, want ready server", server, err)
				}
				maxCalls := max(tc.wantCalls, tc.wantMaxCalls)
				if elapsed := time.Since(started); elapsed != tc.wantElapsed || calls < tc.wantCalls || calls > maxCalls {
					t.Fatalf("elapsed=%s calls=%d, want %s/%d..%d", elapsed, calls, tc.wantElapsed, tc.wantCalls, maxCalls)
				}
			})
		})
	}
}

func TestWaitForServerIPPreservesCallerCauseAndClassification(t *testing.T) {
	for _, phase := range []string{"before read", "read", "wait"} {
		t.Run(phase, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				cause := core.Exit(7, "caller stopped GCP readiness")
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				if phase == "before read" {
					cancel(cause)
				}
				if phase == "wait" {
					time.AfterFunc(time.Second, func() { cancel(cause) })
				}
				calls := 0
				client := &fakeGCPDoctorClient{observe: func(observeCtx context.Context, _ string) (core.Server, error) {
					calls++
					if phase == "read" {
						cancel(cause)
						return core.Server{}, observeCtx.Err()
					}
					return core.Server{}, nil
				}}
				_, err := waitForServerIP(ctx, client, "readiness-instance")
				wantCalls := 1
				if phase == "before read" {
					wantCalls = 0
				}
				if !errors.Is(err, cause) || !errors.Is(err, context.Canceled) || err.Error() != cause.Error() || core.ExitCodeForError(err, 1) != 7 || calls != wantCalls {
					t.Fatalf("err=%v code=%d calls=%d wantCalls=%d", err, core.ExitCodeForError(err, 1), calls, wantCalls)
				}
				got := core.FinalizeRunResult(core.RunResult{}, err)
				want := core.FinalizeRunResult(core.RunResult{}, ctx.Err())
				if got.Status != want.Status || got.ErrorKind != want.ErrorKind {
					t.Fatalf("classification=%s/%s want=%s/%s", got.Status, got.ErrorKind, want.Status, want.ErrorKind)
				}
			})
		})
	}
}

func TestWaitForServerIPRealHTTPS(t *testing.T) {
	for _, ownedBudget := range []bool{false, true} {
		t.Run(fmt.Sprintf("owned_budget=%t", ownedBudget), func(t *testing.T) {
			if ownedBudget && testing.Short() {
				t.Skip("exercises the real two-minute readiness budget")
			}
			testutil.IsolateUserDirs(t)
			credentials := filepath.Join(t.TempDir(), "synthetic-adc.json")
			if err := os.WriteFile(credentials, []byte(`{"type":"authorized_user","client_id":"fixture","client_secret":"fixture","refresh_token":"fixture"}`), 0600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", credentials)
			t.Setenv("GOOGLE_API_USE_CLIENT_CERTIFICATE", "false")
			t.Setenv("GOOGLE_API_USE_MTLS_ENDPOINT", "never")
			received := make(chan struct{}, 1)
			requestCanceled := make(chan struct{}, 1)
			release := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/token" {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"access_token":"fixture","token_type":"Bearer","expires_in":3600}`)
					return
				}
				if r.Method != http.MethodGet || r.URL.Path != "/compute/v1/projects/project/zones/us-central1-b/instances/readiness-instance" || r.TLS == nil {
					t.Errorf("unexpected SDK request: %s %s TLS=%t", r.Method, r.URL.Path, r.TLS != nil)
					http.Error(w, "unexpected request", http.StatusBadRequest)
					return
				}
				select {
				case received <- struct{}{}:
				default:
				}
				select {
				case <-r.Context().Done():
					select {
					case requestCanceled <- struct{}{}:
					default:
					}
					// Returning here can race cancellation with an implicit empty 200.
					<-release
				case <-release:
				}
			}))
			defer server.Close()
			defer close(release)
			// The SDK clones *http.Transport; a RoundTripper wrapper falls back
			// to its default transport and would not isolate the real client.
			transport := server.Client().Transport.(*http.Transport).Clone()
			transport.Proxy = nil
			transport.TLSClientConfig.ServerName = "example.com"
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				switch address {
				case "oauth2.googleapis.com:443", "compute.googleapis.com:443":
					return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
				default:
					return nil, fmt.Errorf("unexpected SDK destination %q", address)
				}
			}
			previous := http.DefaultTransport
			http.DefaultTransport = transport
			defer func() { http.DefaultTransport = previous; transport.CloseIdleConnections() }()
			guard, stopGuard := context.WithTimeout(t.Context(), 3*time.Minute)
			defer stopGuard()
			client, err := core.NewGCPClient(guard, core.Config{GCP: core.GCPConfig{Project: "project", Zone: "us-central1-b"}})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancelCause(guard)
			defer cancel(nil)
			type result struct {
				server core.Server
				err    error
			}
			done := make(chan result, 1)
			started := time.Now()
			go func() {
				got, err := waitForServerIP(ctx, client, "readiness-instance")
				done <- result{got, err}
			}()
			select {
			case <-received:
			case got := <-done:
				t.Fatalf("readiness returned before the SDK request reached the server: %v", got.err)
			case <-time.After(10 * time.Second):
				t.Fatal("SDK request did not reach the HTTPS server")
			}
			cause := core.Exit(7, "caller stopped GCP readiness")
			wantIdentity, wantCode, wantText := error(context.Canceled), 7, cause.Error()
			if ownedBudget {
				wantIdentity, wantCode, wantText = context.DeadlineExceeded, 1, "timeout waiting for gcp public ip on readiness-instance"
			} else {
				cancel(cause)
			}
			select {
			case got := <-done:
				if !reflect.DeepEqual(got.server, core.Server{}) || !errors.Is(got.err, wantIdentity) || core.ExitCodeForError(got.err, 1) != wantCode || got.err.Error() != wantText || !ownedBudget && !errors.Is(got.err, cause) {
					t.Fatalf("server=%+v err=%v code=%d; want identity=%v code=%d text=%q", got.server, got.err, core.ExitCodeForError(got.err, 1), wantIdentity, wantCode, wantText)
				}
				if ownedBudget && (ctx.Err() != nil || time.Since(started) < 2*time.Minute) {
					t.Fatalf("owned budget not reached independently of caller: caller=%v elapsed=%s", ctx.Err(), time.Since(started))
				}
				classified := core.FinalizeRunResult(core.RunResult{}, got.err)
				want := core.FinalizeRunResult(core.RunResult{}, wantIdentity)
				if classified.Status != want.Status || classified.ErrorKind != want.ErrorKind {
					t.Fatalf("classification=%s/%s want=%s/%s", classified.Status, classified.ErrorKind, want.Status, want.ErrorKind)
				}
				t.Logf("real HTTPS through NewGCPClient -> GetServer -> waitForServerIP: ownedBudget=%t elapsed=%s identity=%v code=%d diagnostic=%q classification=%s/%s", ownedBudget, time.Since(started), wantIdentity, wantCode, got.err.Error(), classified.Status, classified.ErrorKind)
			case <-guard.Done():
				t.Fatal("readiness did not finish within the test guard")
			}
			select {
			case <-requestCanceled:
			case <-time.After(5 * time.Second):
				t.Fatal("HTTPS server did not observe request cancellation")
			}
		})
	}
}

func TestWaitForServerIPCompletedResponseWinsCancellation(t *testing.T) {
	for _, ready := range []bool{false, true} {
		t.Run(fmt.Sprint(ready), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			responseErr := errors.Join(&googleapi.Error{Code: http.StatusForbidden, Message: "fixture denied"}, context.Canceled)
			client := &fakeGCPDoctorClient{observe: func(context.Context, string) (core.Server, error) {
				cancel()
				if !ready {
					return core.Server{}, responseErr
				}
				server := core.Server{Status: "stopped"}
				server.PublicNet.IPv4.IP = " 192.0.2.10 "
				return server, nil
			}}
			got, err := waitForServerIP(ctx, client, "readiness-instance")
			if ready && (err != nil || got.PublicNet.IPv4.IP != " 192.0.2.10 ") || !ready && err != responseErr {
				t.Fatalf("server=%#v err=%v", got, err)
			}
		})
	}
}

func TestWaitForServerIPAcceptsCompletedReadyResponseAfterBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		client := &fakeGCPDoctorClient{observe: func(context.Context, string) (core.Server, error) {
			time.Sleep(2*time.Minute + time.Second)
			server := core.Server{}
			server.PublicNet.IPv4.IP = "192.0.2.10"
			return server, nil
		}}
		got, err := waitForServerIP(t.Context(), client, "readiness-instance")
		if err != nil || got.PublicNet.IPv4.IP != "192.0.2.10" {
			t.Fatalf("server=%#v err=%v", got, err)
		}
	})
}

func TestGCPAcquireCleansUpCreatedServerOnIPFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	ipErr := errors.New("ip unavailable")
	fake := &fakeGCPDoctorClient{
		created:   core.Server{CloudID: "crabbox-created", Name: "crabbox-created", Labels: map[string]string{"lease": "cbx_created"}},
		createCfg: core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}},
		getErr:    ipErr,
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, ipErr) {
		t.Fatalf("err=%v, want IP failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-created" {
		t.Fatalf("deleted=%v, want created server cleanup", fake.deleted)
	}
	key, err := core.TestboxKeyPath(fake.createLeaseIDs[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Dir(key)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SSH artifacts remain after successful rollback: %v", err)
	}
}

func TestGCPAcquireCleansUpCreatedServerOnFallbackClientFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	rebuildErr := errors.New("fallback auth failed")
	fake := &fakeGCPDoctorClient{
		created:   core.Server{CloudID: "crabbox-fallback", Name: "crabbox-fallback", Labels: map[string]string{"lease": "cbx_created"}},
		createCfg: core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-c"}},
	}
	calls := 0
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		calls++
		if calls >= 2 {
			return nil, rebuildErr
		}
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, rebuildErr) {
		t.Fatalf("err=%v, want fallback client failure", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != "crabbox-fallback" {
		t.Fatalf("deleted=%v, want created server cleanup", fake.deleted)
	}
	if calls < 3 {
		t.Fatalf("newGCPClient calls=%d, want cleanup client rebuild attempt", calls)
	}
}

func TestGCPAcquireRollbackWaitsWithFreshBoundedContextAfterCancellation(t *testing.T) {
	testutil.IsolateUserDirs(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	var cleanupErr error
	var remaining time.Duration
	var bounded bool
	fake := &fakeGCPDoctorClient{
		created:   core.Server{CloudID: "crabbox-created"},
		createCfg: core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-a"}},
		observe: func(context.Context, string) (core.Server, error) {
			cancel()
			return core.Server{}, context.Canceled
		},
		deleteObserve: func(cleanupCtx context.Context, _ string) error {
			cleanupErr = cleanupCtx.Err()
			deadline, ok := cleanupCtx.Deadline()
			bounded = ok
			remaining = time.Until(deadline)
			return nil
		},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, fake.createCfg, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := backend.Acquire(ctx, core.AcquireRequest{})
	if !errors.Is(err, context.Canceled) || len(fake.deleted) != 1 || fake.createCalls != 1 {
		t.Fatalf("error=%v creates=%d deletes=%v", err, fake.createCalls, fake.deleted)
	}
	if cleanupErr != nil || !bounded || remaining < 170*time.Second || remaining > 3*time.Minute {
		t.Fatalf("cleanup context error=%v bounded=%v remaining=%s, want fresh three-minute operation budget", cleanupErr, bounded, remaining)
	}
}

func TestGCPCleanupKeptIdleLeaseAndStaleClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	repo := t.TempDir()
	expiredLeaseID := "cbx_111111111111"
	staleLeaseID := "cbx_222222222222"
	otherProjectLeaseID := "cbx_333333333333"
	if err := core.ClaimLeaseForRepoProviderScope(staleLeaseID, "stale-box", "gcp", "project:project-a", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if err := core.ClaimLeaseForRepoProviderScope(otherProjectLeaseID, "other-box", "gcp", "project:project-b", repo, time.Minute, false); err != nil {
		t.Fatal(err)
	}
	expired := core.Server{
		CloudID: core.LeaseProviderName(expiredLeaseID, "expired-box"),
		Name:    core.LeaseProviderName(expiredLeaseID, "expired-box"),
		ID:      42,
		Labels: map[string]string{
			"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
			"provider_key": "crabbox-test", "lease": expiredLeaseID, "slug": "expired-box", "zone": "us-central1-b",
			"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
			"keep": "true", "idle_timeout_secs": "1800",
		},
	}
	claimGCPTestServer(t, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, expired)
	fake := &fakeGCPDoctorClient{
		servers: []core.Server{expired,
			{
				CloudID: "crabbox-forged",
				Name:    "crabbox-forged",
				ID:      43,
				Labels: map[string]string{
					"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
					"lease": "cbx_444444444444", "slug": "forged",
					"state": "ready", "expires_at": core.LeaseLabelTime(time.Now().Add(-time.Hour)),
				},
			}},
		get: map[string]core.Server{expired.CloudID: expired},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a"}}, core.Runtime{Stderr: &stderr})
	cleaner, ok := backend.(core.CleanupBackend)
	if !ok {
		t.Fatal("gcp backend missing cleanup")
	}
	if err := cleaner.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v, want no mutation", fake.deleted)
	}
	if claim, err := core.ReadLeaseClaim(expiredLeaseID); err != nil || claim.LeaseID == "" {
		t.Fatalf("dry-run exact claim=%+v err=%v, want retained claim", claim, err)
	}
	stderr.Reset()
	if err := cleaner.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != core.LeaseProviderName(expiredLeaseID, "expired-box") {
		t.Fatalf("deleted=%v want canonical expired server", fake.deleted)
	}
	for _, leaseID := range []string{expiredLeaseID, staleLeaseID} {
		claim, err := core.ReadLeaseClaim(leaseID)
		if err != nil {
			t.Fatal(err)
		}
		if claim.LeaseID != "" {
			t.Fatalf("claim %s still present: %#v", leaseID, claim)
		}
	}
	claim, err := core.ReadLeaseClaim(otherProjectLeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("other project claim was removed")
	}
	out := stderr.String()
	if !strings.Contains(out, "delete server id="+core.LeaseProviderName(expiredLeaseID, "expired-box")) ||
		!strings.Contains(out, "remove stale claim lease="+staleLeaseID) {
		t.Fatalf("cleanup output=%q", out)
	}
}

func TestGCPCleanupRevalidatesLiveOwnershipBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_111111111111", "stale")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: snapshot.Labels["zone"]}}
	claimGCPTestServer(t, cfg, snapshot)
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	delete(live.Labels, "created_by")
	fake := &fakeGCPDoctorClient{
		servers:  []core.Server{snapshot},
		complete: []core.Server{live},
		get:      map[string]core.Server{snapshot.CloudID: live},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup crossed changed ownership: deleted=%v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "live instance no longer has canonical Crabbox ownership labels") {
		t.Fatalf("stderr=%q, want changed-ownership skip", stderr.String())
	}
}

func TestGCPCleanupSkipsClaimlessAndStaleExactClaims(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}
	claimless := canonicalGCPTestServer("cbx_333333333333", "claimless")
	claimless.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	stale := canonicalGCPTestServer("cbx_444444444444", "stale")
	stale.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	claimGCPTestServer(t, cfg, stale)
	stale.ID++
	fake := &fakeGCPDoctorClient{
		servers: []core.Server{claimless, stale},
		get:     map[string]core.Server{claimless.CloudID: claimless, stale.CloudID: stale},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted without exact authority: %v", fake.deleted)
	}
	if got := strings.Count(stderr.String(), "reason=exact local claim missing or stale"); got != 2 {
		t.Fatalf("stderr=%q, missing/stale skips=%d want 2", stderr.String(), got)
	}
}

func TestGCPCleanupRetainsClaimWhenOwnershipLabelsDrift(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_555555555555", "drifted")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["created_by"] = "external"
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: snapshot.Labels["zone"]}}
	if err := core.ClaimLeaseTargetForConfig(snapshot.Labels["lease"], snapshot.Labels["slug"], cfg, snapshot, core.SSHTarget{}, time.Hour); err != nil {
		t.Fatal(err)
	}
	fake := &fakeGCPDoctorClient{
		servers:  []core.Server{snapshot},
		complete: []core.Server{live},
		get:      map[string]core.Server{snapshot.CloudID: live},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(snapshot.Labels["lease"])
	if err != nil || !ok || claim.CloudID != snapshot.CloudID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained exact claim", claim, ok, err)
	}
	if !strings.Contains(stderr.String(), "cloud resource still exists") {
		t.Fatalf("stderr=%q, want retained-claim diagnostic", stderr.String())
	}
}

func TestGCPCleanupRevalidatesLiveEligibilityBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	snapshot := canonicalGCPTestServer("cbx_111111111111", "renewed")
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: snapshot.Labels["zone"]}}
	claimGCPTestServer(t, cfg, snapshot)
	live := snapshot
	live.Labels = maps.Clone(snapshot.Labels)
	live.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(time.Hour))
	fake := &fakeGCPDoctorClient{servers: []core.Server{snapshot}, get: map[string]core.Server{snapshot.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup deleted renewed instance: %v", fake.deleted)
	}
	if !strings.Contains(stderr.String(), "reason=live instance") {
		t.Fatalf("stderr=%q, want renewed-live skip", stderr.String())
	}
}

func TestGCPCleanupTreatsMissingLiveInstanceAsAlreadyDeleted(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_111111111111"
	slug := "gone"
	snapshot := canonicalGCPTestServer(leaseID, slug)
	snapshot.Labels["expires_at"] = core.LeaseLabelTime(time.Now().Add(-time.Hour))
	cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: snapshot.Labels["zone"]}}
	claimGCPTestServer(t, cfg, snapshot)
	artifacts := gcpTestConnectionArtifacts(t, leaseID)
	fake := &fakeGCPDoctorClient{
		servers: []core.Server{snapshot},
		getErr:  &googleapi.Error{Code: http.StatusNotFound, Message: "instance gone"},
	}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })

	var stderr bytes.Buffer
	backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if claim, err := core.ReadLeaseClaim(leaseID); err != nil || claim.LeaseID != leaseID {
		t.Fatalf("dry-run missing-instance claim=%+v err=%v, want retained claim", claim, err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("dry-run deleted=%v, want no mutation", fake.deleted)
	}
	if _, err := os.Stat(filepath.Join(artifacts, "id_ed25519")); err != nil {
		t.Fatalf("dry-run changed connection artifacts: %v", err)
	}
	stderr.Reset()
	if err := backend.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("cleanup retried delete for missing instance: %v", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("missing-instance claim was not removed: %#v", claim)
	}
	if _, err := os.Lstat(artifacts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing-instance connection artifacts remain: %v", err)
	}
	if !strings.Contains(stderr.String(), "live instance no longer exists") {
		t.Fatalf("stderr=%q, want already-deleted diagnostic", stderr.String())
	}
}

func TestGCPReleaseLeaseRequiresCanonicalLiveOwnership(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_777777777777"
	slug := "release-box"
	live := canonicalGCPTestServer(leaseID, slug)
	claimGCPTestServer(t, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, live)
	artifacts := gcpTestConnectionArtifacts(t, leaseID)
	fake := &fakeGCPDoctorClient{get: map[string]core.Server{live.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: leaseID,
			Server:  live,
		},
	})
	if err != nil {
		t.Fatalf("ReleaseLease() error=%v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != live.CloudID {
		t.Fatalf("deleted=%v want %s", fake.deleted, live.CloudID)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID != "" {
		t.Fatalf("claim still present after verified release: %#v", claim)
	}
	if _, err := os.Lstat(artifacts); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("released connection artifacts remain: %v", err)
	}
}

func TestGCPAbsentLeaseConnectionCleanup(t *testing.T) {
	for _, mode := range []string{"release", "stale cleanup", "changed stale claim"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			leaseID := "cbx_777777777777"
			server := canonicalGCPTestServer(leaseID, "gone")
			cfg := core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}
			claimGCPTestServer(t, cfg, server)
			claim, err := core.ReadLeaseClaim(leaseID)
			if err != nil {
				t.Fatal(err)
			}
			artifacts := gcpTestConnectionArtifacts(t, leaseID)
			fake := &fakeGCPDoctorClient{getErr: &googleapi.Error{Code: http.StatusNotFound}}
			if mode == "changed stale claim" {
				updates := 0
				fake.observe = func(context.Context, string) (core.Server, error) {
					updates++
					labels := maps.Clone(server.Labels)
					labels["cleanup_generation"] = fmt.Sprint(updates)
					if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
						t.Fatal(err)
					}
					return core.Server{}, &googleapi.Error{Code: http.StatusNotFound}
				}
			}
			old := newGCPClient
			newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
			t.Cleanup(func() { newGCPClient = old })
			backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
			if mode == "release" {
				err = backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: core.LeaseTarget{LeaseID: leaseID, Server: server}})
			} else {
				if err := backend.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
					t.Fatal(err)
				}
				if _, statErr := os.Stat(filepath.Join(artifacts, "id_ed25519")); statErr != nil {
					t.Fatalf("dry-run changed SSH artifacts: %v", statErr)
				}
				if mode == "changed stale claim" {
					claim, err = core.ReadLeaseClaim(leaseID)
					if err != nil {
						t.Fatal(err)
					}
				}
				err = backend.Cleanup(context.Background(), core.CleanupRequest{})
			}
			if len(fake.deleted) != 0 {
				t.Fatalf("absent resource was deleted: %v", fake.deleted)
			}
			if mode == "changed stale claim" {
				if err == nil {
					t.Fatal("cleanup accepted a changed claim")
				}
				if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || !exists {
					t.Fatalf("changed claim lost: exists=%v err=%v", exists, readErr)
				}
				if _, statErr := os.Stat(filepath.Join(artifacts, "id_ed25519")); statErr != nil {
					t.Fatalf("changed claim's SSH artifacts lost: %v", statErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, statErr := os.Lstat(artifacts); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("absent lease connection artifacts remain: %v", statErr)
			}
			if _, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil || exists {
				t.Fatalf("absent claim remains: exists=%v err=%v", exists, readErr)
			}
		})
	}
}

func TestGCPReleaseLeaseRefusesWrongLiveLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_888888888888"
	claimed := canonicalGCPTestServer(leaseID, "release-box")
	claimGCPTestServer(t, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, claimed)
	live := claimed
	live.Labels = maps.Clone(claimed.Labels)
	live.Labels["lease"] = "cbx_999999999999"
	live.Labels["slug"] = "other-box"
	live.Name = core.LeaseProviderName(live.Labels["lease"], live.Labels["slug"])
	fake := &fakeGCPDoctorClient{get: map[string]core.Server{claimed.CloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: leaseID,
			Server:  claimed,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "live instance belongs to lease=cbx_999999999999") {
		t.Fatalf("ReleaseLease() error=%v, want wrong-lease refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want no delete", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("claim was removed after refused release")
	}
}

func TestGCPReleaseLeaseRefusesNonCanonicalLiveInstance(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_aaaaaaaaaaaa"
	claimed := canonicalGCPTestServer(leaseID, "release-box")
	cloudID := claimed.CloudID
	claimGCPTestServer(t, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, claimed)
	live := claimed
	live.Labels = maps.Clone(claimed.Labels)
	delete(live.Labels, "created_by")
	fake := &fakeGCPDoctorClient{get: map[string]core.Server{cloudID: live}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	backend := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	err := backend.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{
		Lease: core.LeaseTarget{
			LeaseID: leaseID,
			Server:  claimed,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "not canonical Crabbox-owned") {
		t.Fatalf("ReleaseLease() error=%v, want noncanonical refusal", err)
	}
	if len(fake.deleted) != 0 {
		t.Fatalf("deleted=%v want no delete", fake.deleted)
	}
	claim, err := core.ReadLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.LeaseID == "" {
		t.Fatal("claim was removed after refused release")
	}
}

func TestGCPDoctorListsInventoryOnly(t *testing.T) {
	server := func(leaseID, slug string) core.Server {
		name := core.LeaseProviderName(leaseID, slug)
		return core.Server{
			CloudID: name,
			Name:    name,
			Labels: map[string]string{
				"crabbox": "true", "created_by": "crabbox", "provider": "gcp",
				"lease": leaseID, "slug": slug,
			},
		}
	}
	fake := &fakeGCPDoctorClient{servers: []core.Server{
		server("cbx_555555555555", "one"),
		server("cbx_666666666666", "two"),
		{CloudID: "crabbox-forged", Name: "crabbox-forged", Labels: map[string]string{"crabbox": "true"}},
	}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newGCPClient = old })

	doctor, err := core.ConfigureProviderDoctor(Provider{}, core.Config{}, core.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "gcp" || !strings.Contains(result.Message, "inventory=ready api=list mutation=false leases=2 runtime=unchecked") || !strings.Contains(result.Message, "zone=aggregated") {
		t.Fatalf("result=%#v", result)
	}
	if fake.listCalls != 1 {
		t.Fatalf("list calls=%d, want 1", fake.listCalls)
	}
	if fake.mutated {
		t.Fatal("doctor called a mutating GCP method")
	}
}

func TestGCPAcquireStopsFreshRetryAfterRollbackFailure(t *testing.T) {
	for _, failure := range []string{"", "provider", "artifacts"} {
		t.Run(failure, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CONFIG_HOME", t.TempDir())
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			bootstrapErr := core.Exit(5, "timed out waiting for SSH: fixture readiness failure")
			server := core.Server{CloudID: "crabbox-created", Labels: map[string]string{}}
			server.PublicNet.IPv4.IP = "192.0.2.10"
			fake := &fakeGCPDoctorClient{created: server, get: map[string]core.Server{server.CloudID: server}, createCfg: core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}}
			if failure == "provider" {
				fake.deleteErr = errors.New("delete unavailable")
			}
			oldWait := waitForSSHReady
			waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
				key, err := core.TestboxKeyPath(fake.createLeaseIDs[len(fake.createLeaseIDs)-1])
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Dir(key)
				if err := os.WriteFile(filepath.Join(dir, "known_hosts"), []byte("synthetic host trust\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if failure == "artifacts" {
					if err := os.RemoveAll(dir); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				return bootstrapErr
			}
			t.Cleanup(func() { waitForSSHReady = oldWait })
			old := newGCPClient
			newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
			t.Cleanup(func() { newGCPClient = old })
			var stderr bytes.Buffer
			backend := NewGCPLeaseBackend(core.ProviderSpec{}, fake.createCfg, core.Runtime{Stderr: &stderr}).(*gcpLeaseBackend)
			_, err := backend.Acquire(context.Background(), core.AcquireRequest{})
			want := 2
			if failure != "" {
				want = 1
			}
			if fake.createCalls != want || len(fake.deleted) != want {
				t.Fatalf("creates=%d deletes=%d want=%d error=%v", fake.createCalls, len(fake.deleted), want, err)
			}
			if !core.IsBootstrapWaitError(err) {
				t.Fatalf("lost original bootstrap cause: %v", err)
			}
			if failure == "provider" && !errors.Is(err, fake.deleteErr) {
				t.Fatalf("lost cleanup debt or retried: error=%v stderr=%s", err, stderr.String())
			}
			if failure != "" && strings.Contains(stderr.String(), "retrying with fresh lease") {
				t.Fatalf("retried with cleanup debt: %v", err)
			}
			if failure == "artifacts" && !strings.Contains(err.Error(), "SSH connection artifacts") {
				t.Fatalf("lost artifact cleanup error: %v", err)
			}
			for _, leaseID := range fake.createLeaseIDs {
				key, pathErr := core.TestboxKeyPath(leaseID)
				if pathErr != nil {
					t.Fatal(pathErr)
				}
				_, statErr := os.Lstat(filepath.Dir(key))
				if failure == "" && !errors.Is(statErr, os.ErrNotExist) || failure != "" && statErr != nil {
					t.Fatalf("rollback artifact state: %v, failure=%q", statErr, failure)
				}
			}
		})
	}
}

func TestGCPAcquireRetainsCleanupClientFailureDespiteFallbackSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	primary := core.Exit(5, "timed out waiting for SSH: fixture")
	debt := errors.New("selected-zone cleanup client unavailable")
	server := core.Server{CloudID: "crabbox-created", Labels: map[string]string{}}
	server.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeGCPDoctorClient{created: server, get: map[string]core.Server{server.CloudID: server}, createCfg: core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-c"}}}
	oldClient, oldWait := newGCPClient, waitForSSHReady
	bootstrapReached := false
	newGCPClient = func(ctx context.Context, cfg core.Config) (gcpClient, error) {
		if bootstrapReached {
			if _, bounded := ctx.Deadline(); bounded {
				return nil, debt
			}
		}
		return fake, nil
	}
	waitForSSHReady = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
		bootstrapReached = true
		return primary
	}
	t.Cleanup(func() { newGCPClient = oldClient; waitForSSHReady = oldWait })
	b := NewGCPLeaseBackend(core.ProviderSpec{}, core.Config{Provider: "gcp", GCP: core.GCPConfig{Project: "project-a", Zone: "us-central1-b"}}, core.Runtime{Stderr: io.Discard}).(*gcpLeaseBackend)
	_, err := b.Acquire(context.Background(), core.AcquireRequest{})
	if fake.createCalls != 1 || len(fake.deleted) != 1 || !errors.Is(err, primary) || !errors.Is(err, debt) {
		t.Fatalf("creates=%d deletes=%v error=%v", fake.createCalls, fake.deleted, err)
	}
}

func TestGCPResolvedEndpointDirectAndAlias(t *testing.T) {
	testutil.IsolateUserDirs(t)
	server := canonicalGCPTestServer("cbx_123456abcdef", "example")
	server.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeGCPDoctorClient{servers: []core.Server{server}, get: map[string]core.Server{server.CloudID: server}}
	old := newGCPClient
	newGCPClient = func(context.Context, core.Config) (gcpClient, error) { return fake, nil }
	t.Cleanup(func() { newGCPClient = old })
	for _, id := range []string{server.CloudID, "example"} {
		for _, releaseOnly := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/release=%t", id, releaseOnly), func(t *testing.T) {
				cfg := core.Config{SSHUser: "alice", SSHPort: "2222", SSHKey: "configured-key", TargetOS: "linux"}
				backend := NewGCPLeaseBackend(core.ProviderSpec{}, cfg, core.Runtime{}).(*gcpLeaseBackend)
				got, err := backend.Resolve(t.Context(), core.ResolveRequest{ID: id, ReleaseOnly: releaseOnly})
				if err != nil {
					t.Fatal(err)
				}
				wantHost := "192.0.2.10"
				if got.Server.CloudID != server.CloudID || got.LeaseID != "cbx_123456abcdef" || got.SSH.Host != wantHost || got.SSH.User != "alice" || got.SSH.Port != "2222" || got.SSH.Key != "configured-key" {
					t.Fatalf("resolved target: %#v", got)
				}
			})
		}
	}
}
