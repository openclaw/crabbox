package boxd

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/providers/boxd/boxdapi"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
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

type fakeVM struct {
	ID, Name, Status, PublicIP          string
	Isolated                            bool
	SharedOrg, BillingOrg, BillingOrgID string
}

// fakeAPI implements the boxd gRPC surface Crabbox uses, with production's
// observed semantics: id-addressed reads, NOT_FOUND for unknown ids, and a
// readable "destroyed" tombstone after destroy.
type fakeAPI struct {
	boxdapi.UnimplementedBoxdApiServer
	mu                                         sync.Mutex
	user                                       string
	rows                                       []fakeVM
	calls                                      []string
	hostKey                                    string
	createStatus                               codes.Code
	createMissingID, createUnavailable         bool
	notIsolated, bootstrapFail                 bool
	destroyFail, destroyRemains, destroyVanish bool
	beforeCreate                               func()
}

func (f *fakeAPI) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}
func (f *fakeAPI) mutate(fn func()) { f.mu.Lock(); defer f.mu.Unlock(); fn() }
func (f *fakeAPI) count(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == call {
			n++
		}
	}
	return n
}
func (f *fakeAPI) find(id string) (int, bool) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			return i, true
		}
	}
	return 0, false
}

func authorized(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if values := md.Get("authorization"); len(values) != 1 || values[0] != "Bearer "+testJWT {
		return status.Error(codes.Unauthenticated, "missing exchanged session")
	}
	return nil
}

func (f *fakeAPI) Whoami(ctx context.Context, _ *boxdapi.WhoamiRequest) (*boxdapi.WhoamiResponse, error) {
	f.record("Whoami")
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return &boxdapi.WhoamiResponse{UserId: f.user}, nil
}

func (f *fakeAPI) GetVm(ctx context.Context, req *boxdapi.GetVmRequest) (*boxdapi.GetVmResponse, error) {
	f.record("GetVm " + req.GetVmId())
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, found := f.find(req.GetVmId())
	if !found {
		return nil, status.Error(codes.NotFound, "no such vm")
	}
	vm := f.rows[i]
	return &boxdapi.GetVmResponse{
		VmId: vm.ID, Name: vm.Name, PublicIp: vm.PublicIP, Status: vm.Status,
		Isolated: vm.Isolated, Org: vm.SharedOrg, BillingOrg: vm.BillingOrg, BillingOrgId: vm.BillingOrgID,
	}, nil
}

func (f *fakeAPI) CreateVm(ctx context.Context, req *boxdapi.CreateVmRequest) (*boxdapi.CreateVmResponse, error) {
	f.record("CreateVm")
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	hook := f.beforeCreate
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createStatus != codes.OK {
		return nil, status.Error(f.createStatus, "create refused; details include a session echo "+testJWT)
	}
	if f.createUnavailable {
		return nil, status.Error(codes.Unavailable, "connection reset")
	}
	if !req.GetIsolated() || req.GetOrg() != "" {
		return nil, status.Error(codes.InvalidArgument, "unexpected create request shape")
	}
	vm := fakeVM{ID: "vm-1", Name: req.GetName(), Status: "running", PublicIP: "192.0.2.1", Isolated: !f.notIsolated}
	f.rows = append(f.rows, vm)
	id := vm.ID
	if f.createMissingID {
		id = ""
	}
	return &boxdapi.CreateVmResponse{VmId: id, Name: vm.Name, PublicIp: vm.PublicIP, Status: vm.Status, Url: vm.Name + ".boxd.sh"}, nil
}

func (f *fakeAPI) StartVm(ctx context.Context, req *boxdapi.StartVmRequest) (*boxdapi.StartVmResponse, error) {
	f.record("StartVm " + req.GetVmId())
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, found := f.find(req.GetVmId()); found {
		f.rows[i].Status = "running"
	}
	return &boxdapi.StartVmResponse{}, nil
}

func (f *fakeAPI) StopVm(ctx context.Context, req *boxdapi.StopVmRequest) (*boxdapi.StopVmResponse, error) {
	f.record("StopVm " + req.GetVmId())
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if i, found := f.find(req.GetVmId()); found {
		f.rows[i].Status = "stopped"
	}
	return &boxdapi.StopVmResponse{}, nil
}

func (f *fakeAPI) DestroyVm(ctx context.Context, req *boxdapi.DestroyVmRequest) (*boxdapi.DestroyVmResponse, error) {
	f.record("DestroyVm " + req.GetVmId())
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.destroyFail {
		return nil, status.Error(codes.Internal, "destroy failed")
	}
	i, found := f.find(req.GetVmId())
	if !found {
		return nil, status.Error(codes.NotFound, "no such vm")
	}
	switch {
	case f.destroyRemains:
	case f.destroyVanish:
		f.rows = append(f.rows[:i], f.rows[i+1:]...)
	default:
		// Production keeps a readable tombstone and frees the name.
		f.rows[i].Status = "destroyed"
		f.rows[i].Name = f.rows[i].Name + ":" + f.rows[i].ID
	}
	return &boxdapi.DestroyVmResponse{}, nil
}

func (f *fakeAPI) ExposePort(ctx context.Context, req *boxdapi.ExposePortRequest) (*boxdapi.ExposePortResponse, error) {
	f.record("ExposePort " + req.GetVm())
	if err := authorized(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, found := f.find(req.GetVm())
	if !found {
		return nil, status.Error(codes.NotFound, "no such vm")
	}
	if req.GetVmPort() != 2222 || req.GetProtocol() != "tcp" {
		return nil, status.Error(codes.InvalidArgument, "unexpected forward request")
	}
	return &boxdapi.ExposePortResponse{VmName: f.rows[i].Name, Dns: f.rows[i].Name + ".boxd.sh", PublicPort: 32222, VmPort: 2222, Protocol: "tcp", VmId: f.rows[i].ID}, nil
}

func (f *fakeAPI) Exec(stream grpc.BidiStreamingServer[boxdapi.ExecChunk, boxdapi.ExecChunk]) error {
	if err := authorized(stream.Context()); err != nil {
		return err
	}
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	f.record("Exec " + first.GetVmId())
	f.mu.Lock()
	fail := f.bootstrapFail
	hostKey := f.hostKey
	f.mu.Unlock()
	if fail {
		if err := stream.Send(&boxdapi.ExecChunk{Data: []byte("bootstrap refused\n"), IsStderr: true}); err != nil {
			return err
		}
		return stream.Send(&boxdapi.ExecChunk{ExitCode: 1})
	}
	if err := stream.Send(&boxdapi.ExecChunk{Data: []byte("\r\n" + hostKeyMarker + hostKey + "\r\n")}); err != nil {
		return err
	}
	return stream.Send(&boxdapi.ExecChunk{ExitCode: 0})
}

func fixtureBackend(t *testing.T) (*backend, *fakeAPI) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("CRABBOX_BOXD_API_KEY", testAPIKey)
	t.Setenv("BOXD_API_KEY", "")
	f := &fakeAPI{user: "alice", rows: []fakeVM{}, hostKey: fixturePublicKey(t)}
	exchange := httptest.NewTLSServer(validExchange(t, nil))
	t.Cleanup(exchange.Close)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: exchange.TLS.Certificates, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer()
	boxdapi.RegisterBoxdApiServer(server, f)
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.Boxd.APIURL = exchange.URL
	cfg.Boxd.GRPCURL = listener.Addr().String()
	applyDefaults(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{HTTP: exchange.Client(), Stdout: io.Discard, Stderr: io.Discard})
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
func liveRows(f *fakeAPI) []fakeVM {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := []fakeVM{}
	for _, vm := range f.rows {
		if vm.Status != "destroyed" {
			out = append(out, vm)
		}
	}
	return out
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
	if f.count("DestroyVm vm-1") != 1 {
		t.Fatal("expected exact immutable destroy")
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
	if f.count("DestroyVm vm-1") != 0 {
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
			preexisting := fakeVM{ID: "unrelated-vm", Name: "preexisting", Status: "running", PublicIP: "192.0.2.2", Isolated: true}
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
				if f.count("DestroyVm vm-1") != 1 {
					t.Fatal("did not delete exactly the task-created immutable VM")
				}
				f.mutate(func() {
					for _, call := range f.calls {
						if strings.HasPrefix(call, "Exec") || strings.HasPrefix(call, "ExposePort") {
							t.Errorf("guest access before isolation proof: %s", call)
						}
					}
				})
				if live := liveRows(f); len(live) != 1 || live[0] != preexisting {
					t.Errorf("rollback did not preserve only the unchanged preexisting VM: %#v", live)
				}
			}
			if mode == "keep" && f.count("DestroyVm vm-1") != 0 {
				t.Fatal("deleted kept VM")
			}
		})
	}
}
func TestReleaseOwnershipFences(t *testing.T) {
	for _, mode := range []string{"account", "shared", "billing", "org", "origin", "unclaimed", "stale", "same-name"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			lease := acquireFixture(t, b, false)
			switch mode {
			case "account":
				f.mutate(func() { f.user = "mallory" })
			case "shared":
				f.mutate(func() { f.rows[0].SharedOrg = "mallory-org" })
			case "billing":
				f.mutate(func() { f.rows[0].BillingOrg = "mallory-org" })
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
			outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease})
			if outcome.Terminal != (mode == "same-name") {
				t.Errorf("release outcome=%+v mode=%s", outcome, mode)
			}
			if mode == "same-name" {
				if err != nil {
					t.Fatal(err)
				}
				assertNoClaims(t)
			} else if err == nil {
				t.Fatal("ownership fence accepted")
			}
			if f.count("DestroyVm vm-1") != 0 {
				t.Fatal("crossed ownership fence")
			}
		})
	}
}
func TestDeletionRequiresTombstoneOrSustainedAbsence(t *testing.T) {
	b, f := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	f.mutate(func() { f.destroyRemains = true })
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected bounded verification: %v", err)
	}
	onlyClaim(t)
	// The tombstone that production leaves behind is definitive proof.
	f.mutate(func() { f.destroyRemains = false })
	resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatal(err)
	}
	assertNoClaims(t)
	f.mutate(func() {
		if _, found := f.find("vm-1"); !found || f.rows[0].Status != "destroyed" {
			t.Errorf("expected a destroyed tombstone: %#v", f.rows)
		}
	})
}
func TestRetainedReuseAndTouchIdleOverride(t *testing.T) {
	b, f := fixtureBackend(t)
	b.cfg.Boxd.DeleteOnRelease = false
	lease := acquireFixture(t, b, true)
	if outcome, err := b.ReleaseLeaseWithOutcome(context.Background(), core.ReleaseLeaseRequest{Lease: lease}); err != nil || outcome.Terminal {
		t.Fatalf("stop outcome=%+v err=%v", outcome, err)
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
	if f.count("StartVm vm-1") != 1 {
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
	before := f.count("DestroyVm vm-1")
	prior := onlyClaim(t)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if f.count("DestroyVm vm-1") != before || onlyClaim(t).Revision != prior.Revision {
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
	execCalls := f.count("Exec vm-1")
	_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if onlyClaim(t).Revision != before.Revision || f.count("Exec vm-1") != execCalls || f.count("StartVm vm-1") != 0 {
		t.Fatal("status probe mutated lease")
	}
}
func TestUnclaimedNamesCannotBeAdoptedAndRenameKeepsID(t *testing.T) {
	b, f := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	f.mutate(func() {
		f.rows[0].Name = "renamed"
		f.rows = append(f.rows, fakeVM{ID: "foreign", Name: "crabbox-other", Status: "running", PublicIP: "192.0.2.3", Isolated: true})
	})
	if _, err := b.Resolve(context.Background(), core.ResolveRequest{ID: "crabbox-other"}); err == nil {
		t.Fatal("adopted unclaimed machine")
	}
	resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true})
	if err != nil || resolved.Server.CloudID != "vm-1" {
		t.Fatal(err)
	}
	// Vanish on destroy: deletion proof must also work through sustained
	// absence when no tombstone is left behind.
	f.mutate(func() { f.destroyVanish = true })
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
	for _, mode := range []string{"stale", "account", "billing", "missing"} {
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
				case "billing":
					f.rows[0].BillingOrg = "other-org"
				case "missing":
					f.rows = []fakeVM{}
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

func TestTouchCancelsBehindClaimOwner(t *testing.T) {
	b, _ := fixtureBackend(t)
	lease := acquireFixture(t, b, false)
	before := onlyClaim(t)
	done := make(chan error, 1)
	joined := false
	err := core.WithLeaseClaimUnchanged(lease.LeaseID, before, func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		go func() {
			_, err := b.Touch(ctx, core.TouchRequest{Lease: lease, State: "running"})
			done <- err
		}()
		select {
		case err := <-done:
			joined = true
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("canceled touch returned %v", err)
			}
		case <-time.After(time.Second):
			t.Error("canceled touch remained blocked behind the claim owner")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !joined {
		<-done
	}
	if onlyClaim(t).Revision != before.Revision {
		t.Fatal("canceled touch changed the owned claim")
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
	if f.count("DestroyVm vm-1") != 0 {
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
	if onlyClaim(t).Revision != prior.Revision || f.count("StartVm vm-1") != 0 {
		t.Fatal("read-only probe restarted VM")
	}
}

func TestCreateRejectionRemovesOnlyPendingClaim(t *testing.T) {
	for _, code := range []codes.Code{codes.InvalidArgument, codes.Unauthenticated, codes.PermissionDenied, codes.ResourceExhausted, codes.Internal} {
		t.Run(code.String(), func(t *testing.T) {
			b, f := fixtureBackend(t)
			f.createStatus = code
			_, err := b.Acquire(context.Background(), core.AcquireRequest{Keep: true})
			noSecrets(t, err)
			if code == codes.Internal {
				onlyClaim(t)
				if !strings.Contains(err.Error(), "ambiguous") {
					t.Fatal("server error lost recovery guidance")
				}
			} else {
				assertNoClaims(t)
				if strings.Contains(err.Error(), "ambiguous") {
					t.Fatal("definite rejection reported as ambiguous")
				}
				if (code == codes.Unauthenticated || code == codes.PermissionDenied) && !strings.Contains(err.Error(), "BOXD_API_KEY") {
					t.Fatal("missing API-key guidance")
				}
			}
			if f.count("CreateVm") != 1 || f.count("DestroyVm vm-1") != 0 {
				t.Fatal("retried or attempted cleanup of rejected create")
			}
		})
	}
}
func TestAmbiguousCreateFailuresRetainIntent(t *testing.T) {
	for _, mode := range []string{"unavailable", "missing-id"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			f.createUnavailable = mode == "unavailable"
			f.createMissingID = mode == "missing-id"
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
	f.mutate(func() { f.createStatus = codes.Unauthenticated })
	_, err := b.Acquire(context.Background(), core.AcquireRequest{RequestedSlug: "rejected"})
	if err == nil || strings.Contains(err.Error(), "ambiguous") {
		t.Fatal("auth rejection was not reported directly")
	}
	after := onlyClaim(t)
	if after.LeaseID != existing.LeaseID || after.Revision != before.Revision {
		t.Fatal("auth rejection changed an existing claim")
	}
	if f.count("DestroyVm vm-1") != 0 {
		t.Fatal("auth rejection deleted existing VM")
	}
}

// rewriteClaimScope rewrites the stored claim to the exact serialization the
// earlier console-based provider used, simulating an upgrade over it.
func rewriteClaimScope(t *testing.T, leaseID, scope string) {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(os.Getenv("XDG_STATE_HOME"), "crabbox", "claims", "*.json"))
	if err != nil || len(files) != 1 {
		t.Fatalf("claims=%v err=%v", files, err)
	}
	data, err := os.ReadFile(files[0])
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if raw["lease_id"] != leaseID && raw["LeaseID"] != leaseID {
		// Field naming is an implementation detail; find the scope key by value.
		found := false
		for key, value := range raw {
			if s, ok := value.(string); ok && s == leaseID {
				found = true
				_ = key
			}
		}
		if !found {
			t.Fatalf("claim file does not reference lease %s: %s", leaseID, data)
		}
	}
	rewritten := false
	for key, value := range raw {
		if s, ok := value.(string); ok && json.Valid([]byte(s)) && strings.HasPrefix(s, "[\"") {
			raw[key] = scope
			rewritten = true
		}
	}
	if !rewritten {
		t.Fatalf("no provider scope found in claim: %s", data)
	}
	out, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files[0], out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyClaimsRemainVisibleForCleanupAndRelease(t *testing.T) {
	for _, mode := range []string{"release", "cleanup", "guest-refused", "account-fence"} {
		t.Run(mode, func(t *testing.T) {
			b, f := fixtureBackend(t)
			lease := acquireFixture(t, b, false)
			legacy := legacyClaimScope(b.cfg)
			if legacy == b.scope() {
				t.Fatal("legacy scope must differ from the current scope")
			}
			rewriteClaimScope(t, lease.LeaseID, legacy)
			claim := onlyClaim(t)
			if claim.ProviderScope != legacy {
				t.Fatalf("scope rewrite failed: %q", claim.ProviderScope)
			}
			views, err := b.List(context.Background(), core.ListRequest{})
			if err != nil || len(views) != 1 {
				t.Fatalf("legacy claim invisible: views=%d err=%v", len(views), err)
			}
			switch mode {
			case "release":
				resolved, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReleaseOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err != nil {
					t.Fatal(err)
				}
				assertNoClaims(t)
				if f.count("DestroyVm vm-1") != 1 {
					t.Fatal("legacy release did not destroy the exact immutable VM")
				}
			case "cleanup":
				// A failed-cleanup claim from before the migration must stay
				// reachable by cleanup after the upgrade.
				failed := map[string]string{}
				for k, v := range claim.Labels {
					failed[k] = v
				}
				failed["recovery"] = "failed"
				if _, err := core.UpdateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, failed); err != nil {
					t.Fatal(err)
				}
				if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
					t.Fatal(err)
				}
				assertNoClaims(t)
				if f.count("DestroyVm vm-1") != 1 {
					t.Fatal("legacy cleanup did not destroy the exact immutable VM")
				}
			case "guest-refused":
				_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID})
				if err == nil || !strings.Contains(err.Error(), "predates") {
					t.Fatalf("legacy claim allowed guest access: %v", err)
				}
				_, err = b.Resolve(context.Background(), core.ResolveRequest{ID: lease.LeaseID, StatusOnly: true, ReadyProbe: true})
				if err == nil || !strings.Contains(err.Error(), "predates") {
					t.Fatalf("legacy claim allowed a ready probe: %v", err)
				}
				onlyClaim(t)
			case "account-fence":
				f.mutate(func() { f.user = "mallory" })
				resolved := core.LeaseTarget{LeaseID: lease.LeaseID, Server: lease.Server}
				if err := b.ReleaseLease(context.Background(), core.ReleaseLeaseRequest{Lease: resolved}); err == nil {
					t.Fatal("legacy claim crossed the account fence")
				}
				if f.count("DestroyVm vm-1") != 0 {
					t.Fatal("fenced legacy release mutated the machine")
				}
				onlyClaim(t)
			}
		})
	}
	// New acquisitions must always write the current scope.
	b, _ := fixtureBackend(t)
	acquireFixture(t, b, false)
	if onlyClaim(t).ProviderScope != b.scope() {
		t.Fatal("acquisition wrote a non-current scope")
	}
}
