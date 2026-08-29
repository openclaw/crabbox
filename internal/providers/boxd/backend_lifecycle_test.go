package boxd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"golang.org/x/crypto/ssh"
	"nhooyr.io/websocket"
)

func fixturePublicKey(t *testing.T) string {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
}

type fakeAPI struct {
	createCode                                                               int
	createMalformed, createDisconnect                                        bool
	mu                                                                       sync.Mutex
	user                                                                     string
	rows                                                                     []consoleMachine
	calls                                                                    []string
	hostKey                                                                  string
	createMissingID, notIsolated, bootstrapFail, destroyFail, destroyRemains bool
	beforeCreate                                                             func()
}

func (f *fakeAPI) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if strings.HasSuffix(r.URL.Path, "/term") {
		if r.URL.Query().Get("token") != testJWT {
			w.WriteHeader(401)
			return
		}
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer ws.CloseNow()
		_, _, err = ws.Read(r.Context())
		if err != nil {
			return
		}
		ws.Write(r.Context(), websocket.MessageBinary, []byte("\r\n"+hostKeyMarker+f.hostKey+"\r\n"))
		if f.bootstrapFail {
			ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"exit","code":1}`))
		} else {
			ws.Write(r.Context(), websocket.MessageText, []byte(`{"type":"exit","code":0}`))
		}
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testJWT {
		w.WriteHeader(401)
		return
	}
	switch r.URL.Path {
	case "/api/v1/whoami":
		json.NewEncoder(w).Encode(map[string]string{"user_id": f.user})
	case "/api/v1/vms":
		if r.Method == "GET" {
			json.NewEncoder(w).Encode(f.rows)
			return
		}
		if f.beforeCreate != nil {
			f.beforeCreate()
		}
		if f.createCode != 0 {
			w.WriteHeader(f.createCode)
			io.WriteString(w, "console mutations require an interactive session — API-key and in-VM sessions are read-only here "+testJWT)
			return
		}
		if f.createMalformed {
			io.WriteString(w, "unsafe-body")
			return
		}
		if f.createDisconnect {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		var body struct {
			Name     string `json:"name"`
			Org      string `json:"org"`
			Isolated bool   `json:"isolated"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if !body.Isolated {
			w.WriteHeader(400)
			return
		}
		vm := consoleMachine{ID: "vm-1", Name: body.Name, Status: "running", PublicIP: "192.0.2.1", OwnerID: f.user, Isolated: !f.notIsolated}
		f.rows = append(f.rows, vm)
		if f.createMissingID {
			vm.ID = ""
		}
		json.NewEncoder(w).Encode(map[string]string{
			"name": vm.Name, "public_ip": vm.PublicIP, "status": vm.Status,
			"url": "https://" + vm.Name + ".boxd.sh", "vm_id": vm.ID,
		})
	case "/api/v1/vms/vm-1/expose":
		var body consoleForward
		json.NewDecoder(r.Body).Decode(&body)
		if body.VMPort != 2222 || body.Protocol != "tcp" {
			w.WriteHeader(400)
			return
		}
		json.NewEncoder(w).Encode(consoleForward{Endpoint: "https://untrusted.invalid/?token=" + testJWT, PublicPort: 32222, VMPort: 2222, Protocol: "tcp"})
	case "/api/v1/vms/vm-1/start":
		for i := range f.rows {
			if f.rows[i].ID == "vm-1" {
				f.rows[i].Status = "running"
			}
		}
		w.WriteHeader(204)
	case "/api/v1/vms/vm-1/stop":
		for i := range f.rows {
			if f.rows[i].ID == "vm-1" {
				f.rows[i].Status = "stopped"
			}
		}
		w.WriteHeader(204)
	case "/api/v1/vms/vm-1/destroy":
		if f.destroyFail {
			w.WriteHeader(500)
			return
		}
		if !f.destroyRemains {
			out := make([]consoleMachine, 0)
			for _, vm := range f.rows {
				if vm.ID != "vm-1" {
					out = append(out, vm)
				}
			}
			f.rows = out
		}
		w.WriteHeader(204)
	default:
		w.WriteHeader(404)
	}
}
func (f *fakeAPI) mutate(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }
func (f *fakeAPI) count(route string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == route {
			n++
		}
	}
	return n
}
func fixtureBackend(t *testing.T) (*backend, *fakeAPI) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_BOXD_TOKEN", testJWT)
	t.Setenv("BOXD_TOKEN", testJWT)
	f := &fakeAPI{user: "alice", rows: []consoleMachine{}, hostKey: fixturePublicKey(t)}
	server := httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(server.Close)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Boxd.APIURL = server.URL
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard})
	b.readyTimeout = 3 * time.Second
	b.pollInterval = time.Millisecond
	b.absenceGrace = 3 * time.Millisecond
	b.rollbackTimeout = 500 * time.Millisecond
	b.waitSSH = func(_ context.Context, target *core.SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if target.Host != "192.0.2.1" || target.Port != "32222" || target.User != "boxd" || target.SSHHostKey != f.hostKey || len(target.FallbackPorts) != 0 || target.DisableHostKeyChecking || target.ReadyCheck != boxdReadyCheck {
			t.Errorf("bad SSH target: %#v", target)
		}
		data, err := os.ReadFile(target.KnownHostsFile)
		if err != nil || !strings.Contains(string(data), f.hostKey) {
			t.Errorf("authoritative key not installed before SSH: %v", err)
		}
		return nil
	}
	return b, f
}
func acquireFixture(t *testing.T, b *backend, keep bool) core.LeaseTarget {
	t.Helper()
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "testbox", Keep: keep})
	if err != nil {
		t.Fatal(err)
	}
	return lease
}
func onlyClaim(t *testing.T) core.LeaseClaim {
	t.Helper()
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 1 {
		t.Fatalf("claims=%d err=%v", len(claims), err)
	}
	return claims[0]
}
func assertNoClaims(t *testing.T) {
	t.Helper()
	claims, err := core.ListLeaseClaims()
	if err != nil || len(claims) != 0 {
		t.Fatalf("remaining claims=%d err=%v", len(claims), err)
	}
}

func TestAcquirePinsHostKeyAndClaimsBeforeCreate(t *testing.T) {
	b, f := fixtureBackend(t)
	f.beforeCreate = func() {
		var claim core.LeaseClaim
		files, _ := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", "*.json"))
		if len(files) != 1 {
			t.Errorf("intent not published before create: %v", files)
			return
		}
		data, err := os.ReadFile(files[0])
		if err != nil || json.Unmarshal(data, &claim) != nil {
			t.Error("invalid intent")
			return
		}
		if claim.CloudID != "" || claim.Labels["user_id"] != "alice" {
			t.Error("intent missing before create")
		}
	}
	lease := acquireFixture(t, b, false)
	claim := onlyClaim(t)
	if claim.CloudID != "vm-1" || claim.Labels["recovery"] != "" {
		t.Fatalf("claim=%#v", claim)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t)
	if f.count("POST /api/v1/vms/vm-1/destroy") != 1 {
		t.Fatal("expected exact immutable destroy")
	}
}
func TestBootstrapScriptUsesOnlyLeaseKey(t *testing.T) {
	key := fixturePublicKey(t)
	command, err := bootstrapCommand(key)
	if err != nil {
		t.Fatal(err)
	}
	encoded := strings.Split(strings.Split(command, "printf %s ")[1], " ")[0]
	script, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"PasswordAuthentication no", "KbdInteractiveAuthentication no", "AuthenticationMethods publickey", "AllowUsers boxd", "AuthorizedKeysFile /etc/crabbox-ssh/authorized_keys", "HostKey /etc/ssh/ssh_host_ed25519_key", "sudo -n bash"} {
		if !strings.Contains(string(script)+command, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(command, hostKeyMarker) {
		t.Fatal("echo could spoof host-key marker")
	}
}
func TestAmbiguousCreateNeverAdoptsByName(t *testing.T) {
	b, f := fixtureBackend(t)
	f.createMissingID = true
	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "testbox"})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal(err)
	}
	claim := onlyClaim(t)
	if claim.CloudID != "" {
		t.Fatal("guessed ID from inventory")
	}
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("adopted name")
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	onlyClaim(t)
	if f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
		t.Fatal("deleted unbound VM")
	}
}
func TestBootstrapFailureRollbackAndRetention(t *testing.T) {
	for _, mode := range []string{"bootstrap", "not-isolated", "cleanup-fails", "keep", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			f.bootstrapFail = mode != "not-isolated"
			f.notIsolated = mode == "not-isolated"
			f.destroyFail = mode == "cleanup-fails"
			preexisting := consoleMachine{ID: "unrelated-vm", Name: "preexisting", Status: "running", PublicIP: "192.0.2.2", OwnerID: "alice", Isolated: true}
			if mode == "not-isolated" {
				f.rows = append(f.rows, preexisting)
				b.ensureKey = func(string) (string, string, error) {
					t.Error("generated a lease key before verifying isolation")
					return "", "", errors.New("unexpected key generation")
				}
				b.waitSSH = func(context.Context, *core.SSHTarget, io.Writer, string, time.Duration) error {
					t.Error("attempted SSH before verifying isolation")
					return errors.New("unexpected SSH readiness probe")
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			req := core.AcquireRequest{Keep: mode == "keep"}
			if mode == "canceled" {
				req.OnAcquired = func(core.LeaseTarget) error { cancel(); return context.Canceled }
			}
			_, err := b.Acquire(ctx, req)
			if err == nil {
				t.Fatal("expected failure")
			}
			if mode == "keep" || mode == "cleanup-fails" {
				claim := onlyClaim(t)
				if claim.CloudID != "vm-1" || claim.Labels["recovery"] != "failed" {
					t.Fatalf("bad recovery claim: %#v", claim)
				}
			} else {
				assertNoClaims(t)
			}
			if mode == "not-isolated" {
				if !strings.Contains(err.Error(), "not isolated") {
					t.Fatalf("expected isolation rejection, got %v", err)
				}
				if f.count("POST /api/v1/vms/vm-1/destroy") != 1 {
					t.Fatal("did not delete exactly the task-created immutable VM")
				}
				f.mutate(func() {
					for _, call := range f.calls {
						if strings.HasSuffix(call, "/term") || strings.HasSuffix(call, "/expose") {
							t.Errorf("guest access before isolation proof: %s", call)
						}
					}
					if len(f.rows) != 1 || f.rows[0] != preexisting {
						t.Errorf("rollback did not preserve only the unchanged preexisting VM: %#v", f.rows)
					}
				})
			}
			if mode == "keep" && f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
				t.Fatal("deleted kept VM")
			}
		})
	}
}
func TestReleaseOwnershipFences(t *testing.T) {
	for _, mode := range []string{"account", "owner", "org", "origin", "unclaimed", "stale", "same-name"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			lease := acquireFixture(t, b, false)
			switch mode {
			case "account":
				f.mutate(func() { f.user = "mallory" })
			case "owner":
				f.mutate(func() { f.rows[0].OwnerID = "mallory" })
			case "org":
				b.cfg.Boxd.Org = "another-org"
			case "origin":
				b.cfg.Boxd.APIURL = "https://another.example"
			case "unclaimed":
				core.RemoveLeaseClaim(lease.LeaseID)
			case "stale":
				claim := onlyClaim(t)
				labels := map[string]string{}
				for k, v := range claim.Labels {
					labels[k] = v
				}
				labels["keep"] = "true"
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels); err != nil {
					t.Fatal(err)
				}
			case "same-name":
				f.mutate(func() { f.rows[0].ID = "vm-replacement" })
			}
			err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
			if mode == "same-name" {
				if err != nil {
					t.Fatal(err)
				}
				assertNoClaims(t)
			} else if err == nil {
				t.Fatal("ownership fence accepted")
			}
			if f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
				t.Fatal("crossed ownership fence")
			}
		})
	}
}
func TestDeletionRequiresInventoryProof(t *testing.T) {
	b, f := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	f.mutate(func() { f.destroyRemains = true })
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded verification: %v", err)
	}
	onlyClaim(t)
}
func TestRetainedReuseAndTouchIdleOverride(t *testing.T) {
	b, f := fixtureBackend(t)
	b.cfg.Boxd.DeleteOnRelease = false
	lease := acquireFixture(t, b, true)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	if !b.RetainLeaseClaimAfterRelease(lease) || onlyClaim(t).Labels["state"] != "stopped" {
		t.Fatal("did not retain stopped claim")
	}
	b.cfg.Boxd.DeleteOnRelease = true
	b.cfg.IdleTimeout = time.Hour
	lease, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/vms/vm-1/start") != 1 {
		t.Fatal("did not restart exact VM")
	}
	prior := onlyClaim(t)
	server, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready"})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClaim(t).IdleTimeoutSeconds != prior.IdleTimeoutSeconds {
		t.Fatal("implicit timeout changed")
	}
	lease.Server = server
	override := 17 * time.Minute
	server, err = b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready", IdleTimeoutOverride: &override})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClaim(t).IdleTimeoutSeconds != 1020 || server.Labels["keep"] != "true" {
		t.Fatal("explicit timeout or keep lost")
	}
}
func TestCleanupDryRunAndClaimSnapshot(t *testing.T) {
	b, f := fixtureBackend(t)
	f.bootstrapFail = true
	f.destroyFail = true
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{}); err == nil {
		t.Fatal("expected failure")
	}
	before := f.count("POST /api/v1/vms/vm-1/destroy")
	prior := onlyClaim(t)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if f.count("POST /api/v1/vms/vm-1/destroy") != before || onlyClaim(t).Revision != prior.Revision {
		t.Fatal("dry run mutated")
	}
	f.mutate(func() { f.destroyFail = false })
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t)
}

func TestReadOnlyStatusWaitDoesNotBootstrapOrTouch(t *testing.T) {
	b, f := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	before := onlyClaim(t)
	termCalls := f.count("GET /api/v1/vms/vm-1/term")
	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClaim(t).Revision != before.Revision || f.count("GET /api/v1/vms/vm-1/term") != termCalls || f.count("POST /api/v1/vms/vm-1/start") != 0 {
		t.Fatal("status probe mutated lease")
	}
}
func TestUnclaimedNamesCannotBeAdoptedAndRenameKeepsID(t *testing.T) {
	b, f := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	f.mutate(func() {
		f.rows[0].Name = "renamed"
		f.rows = append(f.rows, consoleMachine{ID: "foreign", Name: "crabbox-other", OwnerID: "alice", Isolated: true})
	})
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "crabbox-other"}); err == nil {
		t.Fatal("adopted unclaimed machine")
	}
	resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true})
	if err != nil || resolved.Server.CloudID != "vm-1" {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	f.mutate(func() {
		if len(f.rows) != 1 || f.rows[0].ID != "foreign" {
			t.Error("deleted unrelated VM")
		}
	})
}
func TestTouchUsesSnapshotAndAccountFence(t *testing.T) {
	for _, mode := range []string{"stale", "account", "owner", "missing"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			lease := acquireFixture(t, b, false)
			if mode == "stale" {
				if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "ready"}); err != nil {
					t.Fatal(err)
				}
			}
			f.mutate(func() {
				switch mode {
				case "account":
					f.user = "other"
				case "owner":
					f.rows[0].OwnerID = "other"
				case "missing":
					f.rows = []consoleMachine{}
				}
			})
			prior := onlyClaim(t)
			if _, err := b.Touch(context.Background(), core.TouchRequest{Lease: lease, State: "running"}); err == nil {
				t.Fatal("touch crossed fence")
			}
			if onlyClaim(t).Revision != prior.Revision {
				t.Fatal("touch changed failed claim")
			}
		})
	}
}
func TestAcquireWithRepositoryAndReuseRetainsTimeout(t *testing.T) {
	b, _ := fixtureBackend(t)
	repo := core.Repo{Root: t.TempDir()}
	lease, err := b.Acquire(context.Background(), core.AcquireRequest{Repo: repo})
	if err != nil {
		t.Fatal(err)
	}
	prior := onlyClaim(t)
	b.cfg.IdleTimeout = 2 * time.Hour
	_, err = b.Resolve(context.Background(), core.ResolveRequest{Repo: repo, ID: lease.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClaim(t).IdleTimeoutSeconds != prior.IdleTimeoutSeconds {
		t.Fatal("reuse replaced idle timeout")
	}
}

func TestKeptFailureCleanupDoesNotDelete(t *testing.T) {
	b, f := fixtureBackend(t)
	f.bootstrapFail = true
	if _, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true}); err == nil {
		t.Fatal("expected bootstrap failure")
	}
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	onlyClaim(t)
	if f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
		t.Fatal("cleanup deleted a kept failure")
	}
}
func TestReadOnlyStatusDoesNotStartStoppedMachine(t *testing.T) {
	b, f := fixtureBackend(t)
	b.cfg.Boxd.DeleteOnRelease = false
	lease := acquireFixture(t, b, false)
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatal(err)
	}
	prior := onlyClaim(t)
	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
	if err == nil {
		t.Fatal("stopped VM reported ready")
	}
	if onlyClaim(t).Revision != prior.Revision || f.count("POST /api/v1/vms/vm-1/start") != 0 {
		t.Fatal("read-only probe restarted VM")
	}
}

func TestCreateRejectionRemovesOnlyPendingClaim(t *testing.T) {
	for _, code := range []int{400, 401, 403, 422, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			b, f := fixtureBackend(t)
			f.createCode = code
			_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
			noSecrets(t, err)
			if code == 500 {
				onlyClaim(t)
				if !strings.Contains(err.Error(), "ambiguous") {
					t.Fatal("server error lost recovery guidance")
				}
			} else {
				assertNoClaims(t)
				if strings.Contains(err.Error(), "ambiguous") {
					t.Fatal("definite rejection reported as ambiguous")
				}
				if (code == 401 || code == 403) && !strings.Contains(err.Error(), "interactive BOXD_TOKEN") {
					t.Fatal("missing interactive session guidance")
				}
			}
			if f.count("POST /api/v1/vms") != 1 || f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
				t.Fatal("retried or attempted cleanup of rejected create")
			}
		})
	}
}
func TestAmbiguousCreateFailuresRetainIntent(t *testing.T) {
	for _, mode := range []string{"disconnect", "malformed-success"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			f.createDisconnect = mode == "disconnect"
			f.createMalformed = mode == "malformed-success"
			_, err := b.Acquire(context.Background(), core.AcquireRequest{})
			if err == nil || !strings.Contains(err.Error(), "ambiguous") {
				t.Fatal("ambiguous failure lost")
			}
			claim := onlyClaim(t)
			if claim.CloudID != "" {
				t.Fatal("invented immutable identity")
			}
		})
	}
}

func TestCreateAuthRejectionPreservesExistingLease(t *testing.T) {
	b, f := fixtureBackend(t)
	existing := acquireFixture(t, b, false)
	before := onlyClaim(t)
	f.mutate(func() { f.createCode = 401 })
	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "rejected"})
	if err == nil || strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("auth rejection was not reported directly")
	}
	after := onlyClaim(t)
	if after.LeaseID != existing.LeaseID || after.Revision != before.Revision {
		t.Fatal("auth rejection changed an existing claim")
	}
	if f.count("POST /api/v1/vms/vm-1/destroy") != 0 {
		t.Fatal("auth rejection deleted existing VM")
	}
}
