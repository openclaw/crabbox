package hetzner

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeHetznerClient struct {
	servers map[int64]Server
	list    []Server

	createServer Server
	createErr    error
	deleteErr    error
	keyDeleteErr error
	keyCreated   bool

	deletedServers []int64
	deletedKeys    []string
}

func (f *fakeHetznerClient) ListCrabboxServers(context.Context) ([]Server, error) {
	return f.list, nil
}

func (f *fakeHetznerClient) EnsureSSHKey(_ context.Context, name, _ string) (core.SSHKey, bool, error) {
	return core.SSHKey{Name: name}, f.keyCreated, nil
}

func (f *fakeHetznerClient) CreateServerWithFallback(context.Context, Config, string, string, string, bool, func(string, ...any)) (Server, Config, error) {
	return f.createServer, Config{}, f.createErr
}

func (f *fakeHetznerClient) GetServer(_ context.Context, id int64) (Server, error) {
	server, ok := f.servers[id]
	if !ok {
		return Server{}, errors.New("server not found")
	}
	return server, nil
}

func (f *fakeHetznerClient) DeleteServer(_ context.Context, id int64) error {
	f.deletedServers = append(f.deletedServers, id)
	return f.deleteErr
}

func (f *fakeHetznerClient) DeleteSSHKey(_ context.Context, name string) error {
	f.deletedKeys = append(f.deletedKeys, name)
	return f.keyDeleteErr
}

func (f *fakeHetznerClient) SetLabels(context.Context, int64, map[string]string) error {
	return nil
}

func installHetznerTestHooks(t *testing.T, client *fakeHetznerClient) {
	t.Helper()
	oldNewClient := newHetznerClient
	oldNewLeaseID := newLeaseID
	oldEnsureKey := ensureTestboxKeyForConfig
	oldProviderKey := providerKeyForLease
	oldWaitForServerIP := waitForServerIP
	oldWaitForSSHReady := waitForSSHReady
	oldBootstrapWaitTimeout := bootstrapWaitTimeout

	newHetznerClient = func() (hetznerClient, error) { return client, nil }
	newLeaseID = func() string { return "cbx_abcdef123456" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 test", nil
	}
	providerKeyForLease = core.ProviderKeyForLease
	waitForServerIP = func(ctx context.Context, client hetznerClient, id int64) (Server, error) {
		return client.GetServer(ctx, id)
	}
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	bootstrapWaitTimeout = func(Config) time.Duration { return 0 }

	t.Cleanup(func() {
		newHetznerClient = oldNewClient
		newLeaseID = oldNewLeaseID
		ensureTestboxKeyForConfig = oldEnsureKey
		providerKeyForLease = oldProviderKey
		waitForServerIP = oldWaitForServerIP
		waitForSSHReady = oldWaitForSSHReady
		bootstrapWaitTimeout = oldBootstrapWaitTimeout
	})
}

func TestHetznerResolveNumericRejectsUnownedServer(t *testing.T) {
	client := &fakeHetznerClient{servers: map[int64]Server{
		42: {ID: 42, Labels: map[string]string{"crabbox": "true"}},
	}}
	installHetznerTestHooks(t, client)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "42"})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate on non-Crabbox Hetzner server") {
		t.Fatalf("err=%v, want ownership refusal", err)
	}
	if len(client.deletedServers) != 0 {
		t.Fatalf("unexpected deletes: %v", client.deletedServers)
	}
}

func TestHetznerResolveAliasRejectsUnownedServer(t *testing.T) {
	client := &fakeHetznerClient{servers: map[int64]Server{}}
	client.servers[42] = Server{ID: 42, Name: "crabbox-test", Labels: map[string]string{"crabbox": "true", "lease": "cbx_abcdef123456", "slug": "test"}}
	client.list = []Server{client.servers[42]}
	installHetznerTestHooks(t, client)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "test"})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate on non-Crabbox Hetzner server") {
		t.Fatalf("err=%v, want ownership refusal", err)
	}
}

func TestHetznerDeleteRejectsUnownedBeforeClient(t *testing.T) {
	called := false
	oldNewClient := newHetznerClient
	newHetznerClient = func() (hetznerClient, error) {
		called = true
		return &fakeHetznerClient{}, nil
	}
	t.Cleanup(func() { newHetznerClient = oldNewClient })

	err := deleteServer(context.Background(), Config{}, Server{ID: 42, Labels: map[string]string{"crabbox": "true"}})
	if err == nil || !strings.Contains(err.Error(), "refusing to operate on non-Crabbox Hetzner server") {
		t.Fatalf("err=%v, want ownership refusal", err)
	}
	if called {
		t.Fatal("newHetznerClient was called before ownership validation")
	}
}

func TestHetznerDeleteAllowsLegacyServerWithoutProviderLabel(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	client := &fakeHetznerClient{}
	installHetznerTestHooks(t, client)

	server := crabboxHetznerServer(42, leaseID)
	delete(server.Labels, "provider")
	if err := deleteServer(context.Background(), Config{}, server); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
}

func TestHetznerReleaseRetainsClaimUntilKeyDeleteSucceeds(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	keyErr := errors.New("delete key failed")
	client := &fakeHetznerClient{keyDeleteErr: keyErr}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	if err := core.ClaimLeaseForRepoProvider(leaseID, "test", providerName, "/repo", 30*time.Minute, false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if !errors.Is(err, keyErr) {
		t.Fatalf("err=%v, want key delete failure", err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok || claim.LeaseID != leaseID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained claim", claim, ok, err)
	}

	client.deleteErr = errors.New(`hetzner DELETE /servers/42: http 404: {"error":{"code":"not_found"}}`)
	client.keyDeleteErr = nil
	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if err != nil {
		t.Fatalf("retry ReleaseLease: %v", err)
	}
	if len(client.deletedServers) != 2 || client.deletedServers[1] != 42 {
		t.Fatalf("deletedServers=%v, want second delete of 42", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 2 || client.deletedKeys[1] != want {
		t.Fatalf("deletedKeys=%v, want second delete of %s", client.deletedKeys, want)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%+v ok=%v err=%v, want removed claim after retry", claim, ok, err)
	}
}

func TestHetznerReleaseTreatsMissingServerAsGone(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	client := &fakeHetznerClient{deleteErr: errors.New(`hetzner DELETE /servers/42: http 404: {"error":{"code":"not_found"}}`)}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	if err := core.ClaimLeaseForRepoProvider(leaseID, "test", providerName, "/repo", 30*time.Minute, false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if err != nil {
		t.Fatalf("ReleaseLease: %v", err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%+v ok=%v err=%v, want removed claim", claim, ok, err)
	}
}

func TestHetznerReleaseDoesNotTreatBodyMentioned404AsMissingServer(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	deleteErr := errors.New(`hetzner DELETE /servers/42: http 500: {"message":"upstream http 404"}`)
	client := &fakeHetznerClient{deleteErr: deleteErr}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	if err := core.ClaimLeaseForRepoProvider(leaseID, "test", providerName, "/repo", 30*time.Minute, false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("err=%v, want server delete failure", err)
	}
	if len(client.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v, want none", client.deletedKeys)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok || claim.LeaseID != leaseID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained claim", claim, ok, err)
	}
}

func TestHetznerAcquireRollsBackAfterIPWaitFailure(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := crabboxHetznerServer(42, leaseID)
	client := &fakeHetznerClient{
		servers:      map[int64]Server{42: server},
		createServer: server,
		keyCreated:   true,
	}
	installHetznerTestHooks(t, client)
	waitErr := errors.New("ip wait failed")
	waitForServerIP = func(context.Context, hetznerClient, int64) (Server, error) {
		return Server{}, waitErr
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, waitErr) {
		t.Fatalf("err=%v, want ip wait failure", err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
}

func TestHetznerAcquireReportsRollbackFailure(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := crabboxHetznerServer(42, leaseID)
	deleteErr := errors.New("delete failed")
	waitErr := errors.New("ip wait failed")
	client := &fakeHetznerClient{
		servers:      map[int64]Server{42: server},
		createServer: server,
		deleteErr:    deleteErr,
		keyCreated:   true,
	}
	installHetznerTestHooks(t, client)
	waitForServerIP = func(context.Context, hetznerClient, int64) (Server, error) {
		return Server{}, waitErr
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, waitErr) || !errors.Is(err, deleteErr) {
		t.Fatalf("err=%v, want both acquisition and cleanup errors", err)
	}
}

func TestHetznerAcquireDeletesProviderKeyWhenCreateFails(t *testing.T) {
	createErr := errors.New("create failed")
	client := &fakeHetznerClient{createErr: createErr, keyCreated: true}
	installHetznerTestHooks(t, client)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, createErr) {
		t.Fatalf("err=%v, want create failure", err)
	}
	if want := core.ProviderKeyForLease("cbx_abcdef123456"); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
}

func TestHetznerAcquireKeepsExistingProviderKeyWhenCreateFails(t *testing.T) {
	createErr := errors.New("create failed")
	client := &fakeHetznerClient{createErr: createErr}
	installHetznerTestHooks(t, client)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	_, err := backend.acquireOnce(context.Background(), false, "")
	if !errors.Is(err, createErr) {
		t.Fatalf("err=%v, want create failure", err)
	}
	if len(client.deletedKeys) != 0 {
		t.Fatalf("deletedKeys=%v, want none", client.deletedKeys)
	}
}

func crabboxHetznerServer(id int64, leaseID string) Server {
	server := Server{
		ID:     id,
		Name:   "crabbox-test",
		Labels: map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "lease": leaseID},
	}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	return server
}

func installHetznerClaimState(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, ".config"))
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_COORDINATOR_ADMIN_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
}
