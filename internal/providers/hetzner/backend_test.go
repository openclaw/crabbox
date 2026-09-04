package hetzner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
	keyName      string

	deletedServers []int64
	deletedKeys    []string
	labeledServers []int64
}

func (f *fakeHetznerClient) ListCrabboxServers(context.Context) ([]Server, error) {
	return f.list, nil
}

func (f *fakeHetznerClient) EnsureSSHKey(_ context.Context, name, _ string) (core.SSHKey, bool, error) {
	if f.keyName != "" {
		name = f.keyName
	}
	return core.SSHKey{Name: name}, f.keyCreated, nil
}

func (f *fakeHetznerClient) CreateServerWithFallback(_ context.Context, cfg Config, _, _, _ string, _ bool, _ func(string, ...any)) (Server, Config, error) {
	return f.createServer, cfg, f.createErr
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

func (f *fakeHetznerClient) SetLabels(_ context.Context, id int64, _ map[string]string) error {
	f.labeledServers = append(f.labeledServers, id)
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
	if err == nil || !strings.Contains(err.Error(), "lease/server not found") {
		t.Fatalf("err=%v, want filtered inventory miss", err)
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
	installHetznerClaimState(t)

	server := crabboxHetznerServer(42, leaseID)
	delete(server.Labels, "provider")
	seedHetznerClaim(t, server)
	if err := deleteServer(context.Background(), Config{}, server); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
}

func TestHetznerReleaseKeepsServerReachableUntilKeyDeleteSucceeds(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	keyErr := errors.New("delete key failed")
	client := &fakeHetznerClient{keyDeleteErr: keyErr}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	seedHetznerClaim(t, crabboxHetznerServer(42, leaseID))

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if !errors.Is(err, keyErr) {
		t.Fatalf("err=%v, want key delete failure", err)
	}
	if len(client.deletedServers) != 0 {
		t.Fatalf("deletedServers=%v, want server retained for retry", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok || claim.LeaseID != leaseID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained claim", claim, ok, err)
	}

	client.keyDeleteErr = nil
	err = backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if err != nil {
		t.Fatalf("retry ReleaseLease: %v", err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42] after successful key delete", client.deletedServers)
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
	client := &fakeHetznerClient{deleteErr: core.HetznerHTTPError{Method: "DELETE", Path: "/servers/42", StatusCode: 404, Detail: `{"error":{"code":"not_found"}}`}}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	seedHetznerClaim(t, crabboxHetznerServer(42, leaseID))

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

	seedHetznerClaim(t, crabboxHetznerServer(42, leaseID))

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: crabboxHetznerServer(42, leaseID)}})
	if !errors.Is(err, deleteErr) {
		t.Fatalf("err=%v, want server delete failure", err)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s] before server delete", client.deletedKeys, want)
	}
	if claim, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok || claim.LeaseID != leaseID {
		t.Fatalf("claim=%+v ok=%v err=%v, want retained claim", claim, ok, err)
	}
}

func TestHetznerReleaseRejectsMismatchedLeaseBeforeDelete(t *testing.T) {
	server := crabboxHetznerServer(42, "cbx_abcdef123456")
	client := &fakeHetznerClient{}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)
	seedHetznerClaim(t, server)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: "cbx_fedcba654321",
		Server:  server,
	}})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("err=%v, want exact-claim refusal", err)
	}
	if len(client.deletedServers) != 0 || len(client.deletedKeys) != 0 {
		t.Fatalf("deleted servers=%v keys=%v", client.deletedServers, client.deletedKeys)
	}
}

func TestHetznerReleaseRejectsClaimForDifferentServer(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	claimed := crabboxHetznerServer(42, leaseID)
	client := &fakeHetznerClient{}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)
	seedHetznerClaim(t, claimed)

	replacement := crabboxHetznerServer(43, leaseID)
	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: replacement}})
	if err == nil || !strings.Contains(err.Error(), "stale exact local claim") {
		t.Fatalf("err=%v, want stale-claim refusal", err)
	}
	if len(client.deletedServers) != 0 || len(client.deletedKeys) != 0 {
		t.Fatalf("deleted servers=%v keys=%v", client.deletedServers, client.deletedKeys)
	}
}

func TestHetznerReleaseRollsBackExactLeaseAcquiredByBackendBeforeClaim(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := acquiredHetznerTestServer(42, leaseID)
	client := &fakeHetznerClient{
		servers:      map[int64]Server{42: server},
		createServer: server,
		keyCreated:   true,
	}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	lease, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease}); err != nil {
		t.Fatalf("ReleaseLease before claim: %v", err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
		t.Fatalf("deletedServers=%v, want [42]", client.deletedServers)
	}
	if want := core.ProviderKeyForLease(leaseID); len(client.deletedKeys) != 1 || client.deletedKeys[0] != want {
		t.Fatalf("deletedKeys=%v, want [%s]", client.deletedKeys, want)
	}
}

func TestHetznerReleaseRejectsUnclaimedLeaseAcquiredByDifferentBackend(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := acquiredHetznerTestServer(42, leaseID)
	client := &fakeHetznerClient{
		servers:      map[int64]Server{42: server},
		createServer: server,
		keyCreated:   true,
	}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)

	acquiringBackend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	lease, err := acquiringBackend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	otherBackend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	err = otherBackend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: lease})
	if err == nil || !strings.Contains(err.Error(), "no exact local claim") {
		t.Fatalf("err=%v, want exact-claim refusal", err)
	}
	if len(client.deletedServers) != 0 || len(client.deletedKeys) != 0 {
		t.Fatalf("deleted servers=%v keys=%v, want none", client.deletedServers, client.deletedKeys)
	}
}

func TestHetznerCleanupSkipsWeakAndUnclaimedServers(t *testing.T) {
	now := time.Now().UTC()
	weak := crabboxHetznerServer(41, "cbx_111111111111")
	delete(weak.Labels, "created_by")
	weak.Labels["expires_at"] = core.LeaseLabelTime(now.Add(-time.Hour))
	unclaimed := crabboxHetznerServer(42, "cbx_222222222222")
	unclaimed.Labels["expires_at"] = core.LeaseLabelTime(now.Add(-time.Hour))
	claimed := crabboxHetznerServer(43, "cbx_333333333333")
	claimed.Labels["expires_at"] = core.LeaseLabelTime(now.Add(-time.Hour))
	client := &fakeHetznerClient{list: []Server{weak, unclaimed, claimed}}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)
	seedHetznerClaim(t, claimed)

	var stderr strings.Builder
	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: &stderr}).(*hetznerLeaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(client.deletedServers) != 1 || client.deletedServers[0] != claimed.ID {
		t.Fatalf("deletedServers=%v, want [%d]", client.deletedServers, claimed.ID)
	}
	if !strings.Contains(stderr.String(), "canonical Crabbox ownership labels missing") || !strings.Contains(stderr.String(), "exact local claim missing or stale") {
		t.Fatalf("stderr=%q, want ownership skip diagnostics", stderr.String())
	}
	if _, ok, err := core.ResolveLeaseClaim(claimed.Labels["lease"]); err != nil || ok {
		t.Fatalf("claimed cleanup claim ok=%v err=%v, want removed", ok, err)
	}
}

func TestHetznerResolveRequiresExplicitReclaimForUnclaimedServer(t *testing.T) {
	server := crabboxHetznerServer(42, "cbx_abcdef123456")
	client := &fakeHetznerClient{servers: map[int64]Server{42: server}}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)
	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)

	_, err := backend.Resolve(context.Background(), ResolveRequest{ID: "42", Repo: core.Repo{Root: "/repo"}})
	if err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("err=%v, want explicit reclaim refusal", err)
	}
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: "42", Repo: core.Repo{Root: "/repo"}, Reclaim: true}); err != nil {
		t.Fatalf("explicit reclaim resolve: %v", err)
	}
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: "42", NoLocalStateMutations: true}); err != nil {
		t.Fatalf("read-only resolve: %v", err)
	}
}

func TestHetznerResolveAliasAllowsExplicitUpgradeOfLegacyClaim(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := crabboxHetznerServer(42, leaseID)
	delete(server.Labels, "provider")
	client := &fakeHetznerClient{list: []Server{server}}
	installHetznerTestHooks(t, client)
	installHetznerClaimState(t)
	seedHetznerClaim(t, server)
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok {
		t.Fatalf("ResolveLeaseClaim: claim=%+v ok=%v err=%v", claim, ok, err)
	}
	legacy := claim
	legacy.Provider = ""
	legacy.CloudID = ""
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, legacy); err != nil {
		t.Fatalf("replace with legacy claim: %v", err)
	}

	backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: "test", Repo: core.Repo{Root: "/repo"}}); err == nil {
		t.Fatal("legacy alias resolved without explicit reclaim")
	}
	lease, err := backend.Resolve(context.Background(), ResolveRequest{ID: "test", Repo: core.Repo{Root: "/repo"}, Reclaim: true})
	if err != nil {
		t.Fatalf("explicit legacy alias reclaim: %v", err)
	}
	if lease.LeaseID != leaseID || lease.Server.CloudID != "42" {
		t.Fatalf("lease=%+v, want exact legacy server", lease)
	}
}

func TestHetznerAcquireRollsBackAfterIPWaitFailure(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := acquiredHetznerTestServer(42, leaseID)
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
	_, err := backend.acquireOnce(context.Background(), false, "test")
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

func TestHetznerAcquireBindsReadinessToCreatedServer(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	for _, tt := range []struct {
		name   string
		mutate func(*Server)
	}{
		{"different ID", func(s *Server) { s.ID = 43; s.CloudID = "43" }},
		{"missing ID", func(s *Server) { s.ID = 0; s.CloudID = "" }},
		{"inconsistent cloud ID", func(s *Server) { s.CloudID = "43" }},
		{"different name", func(s *Server) { s.Name = "another-resource" }},
		{"missing name", func(s *Server) { s.Name = "" }},
		{"different lease", func(s *Server) { s.Labels["lease"] = "cbx_111111111111" }},
		{"different provider", func(s *Server) { s.Labels["provider"] = "other" }},
		{"missing owner", func(s *Server) { delete(s.Labels, "created_by") }},
		{"different slug", func(s *Server) { s.Labels["slug"] = "other" }},
		{"different key", func(s *Server) { s.Labels["provider_key"] = core.ProviderKeyForLease("cbx_111111111111") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			created := acquiredHetznerTestServer(42, leaseID)
			client := &fakeHetznerClient{createServer: created, keyCreated: true}
			installHetznerTestHooks(t, client)
			waitForServerIP = func(_ context.Context, _ hetznerClient, id int64) (Server, error) {
				if id != 42 {
					t.Fatalf("readiness lookup=%d", id)
				}
				// Deliberately reuse the label map returned at creation: the rollback
				// anchor must not depend on mutable later observations.
				ready := created
				tt.mutate(&ready)
				return ready, nil
			}
			sshCalls := 0
			waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
				sshCalls++
				return errors.New("unexpected SSH")
			}
			backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
			_, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
			if err == nil || !strings.Contains(err.Error(), "readiness") {
				t.Fatalf("err=%v, want readiness binding rejection", err)
			}
			if sshCalls != 0 || len(client.labeledServers) != 0 {
				t.Fatalf("ssh=%d labels=%v", sshCalls, client.labeledServers)
			}
			if len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
				t.Fatalf("deleted=%v, want original server42 only", client.deletedServers)
			}
			if len(client.deletedKeys) != 1 || client.deletedKeys[0] != core.ProviderKeyForLease(leaseID) {
				t.Fatalf("deleted keys=%v", client.deletedKeys)
			}
			if _, ok := backend.acquired.Load(leaseID); ok {
				t.Fatal("mismatched resource published as acquired")
			}
		})
	}
}

func acquiredHetznerTestServer(id int64, leaseID string) Server {
	server := crabboxHetznerServer(id, leaseID)
	server.Name = core.LeaseProviderName(leaseID, "test")
	server.Labels = maps.Clone(server.Labels)
	server.Labels["provider_key"] = core.ProviderKeyForLease(leaseID)
	return server
}

func TestHetznerAcquireRejectsUnboundCreateWithoutDeletingReturnedServer(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	for _, tt := range []struct {
		name   string
		mutate func(*Server)
	}{
		{"missing ID", func(s *Server) { s.ID = 0; s.CloudID = "" }},
		{"contradictory ID", func(s *Server) { s.CloudID = "43" }},
		{"foreign name", func(s *Server) { s.Name = "foreign" }},
		{"foreign lease", func(s *Server) { s.Labels["lease"] = "cbx_111111111111" }},
		{"missing provider", func(s *Server) { delete(s.Labels, "provider") }},
		{"foreign key", func(s *Server) { s.Labels["provider_key"] = core.ProviderKeyForLease("cbx_111111111111") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			created := acquiredHetznerTestServer(42, leaseID)
			tt.mutate(&created)
			client := &fakeHetznerClient{createServer: created, keyCreated: true}
			installHetznerTestHooks(t, client)
			waitForServerIP = func(context.Context, hetznerClient, int64) (Server, error) {
				t.Fatal("unbound creation polled")
				return Server{}, nil
			}
			waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
				t.Fatal("unbound creation reached SSH")
				return nil
			}
			backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
			_, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
			if err == nil || !strings.Contains(err.Error(), "server cleanup withheld") {
				t.Fatalf("err=%v", err)
			}
			if len(client.deletedServers) != 0 || len(client.labeledServers) != 0 {
				t.Fatalf("deleted=%v labeled=%v", client.deletedServers, client.labeledServers)
			}
			if len(client.deletedKeys) != 1 || client.deletedKeys[0] != core.ProviderKeyForLease(leaseID) {
				t.Fatalf("deleted keys=%v, want independently created key only", client.deletedKeys)
			}
		})
	}
}

func TestHetznerAcquireAcceptsNewEndpointAndPreservesReusedKey(t *testing.T) {
	const leaseID = "cbx_abcdef123456"
	for _, failSSH := range []bool{false, true} {
		t.Run(strconv.FormatBool(failSSH), func(t *testing.T) {
			created := acquiredHetznerTestServer(42, leaseID)
			const reusedKey = "shared key / with punctuation"
			created.Labels["provider_key"] = core.DirectLeaseLabels(Config{ProviderKey: reusedKey}, leaseID, "test", providerName, "", false, time.Now())["provider_key"]
			client := &fakeHetznerClient{createServer: created, keyName: reusedKey}
			installHetznerTestHooks(t, client)
			waitForServerIP = func(context.Context, hetznerClient, int64) (Server, error) {
				ready := created
				ready.Labels = maps.Clone(created.Labels)
				ready.PublicNet.IPv4.IP = "203.0.113.11"
				ready.Labels["state"] = "running"
				return ready, nil
			}
			sshErr := errors.New("synthetic SSH failure")
			waitForSSHReady = func(_ context.Context, target *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
				if target.Host != "203.0.113.11" {
					t.Fatalf("SSH host=%s", target.Host)
				}
				if failSSH {
					return sshErr
				}
				return nil
			}
			backend := NewHetznerLeaseBackend(ProviderSpec{}, Config{}, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
			lease, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
			if failSSH {
				if !errors.Is(err, sshErr) || len(client.deletedServers) != 1 || client.deletedServers[0] != 42 {
					t.Fatalf("err=%v deleted=%v", err, client.deletedServers)
				}
			} else if err != nil || lease.Server.ID != 42 || len(client.labeledServers) != 1 || len(client.deletedServers) != 0 {
				t.Fatalf("lease=%+v err=%v labeled=%v deleted=%v", lease, err, client.labeledServers, client.deletedServers)
			}
			if len(client.deletedKeys) != 0 {
				t.Fatalf("reused key deleted: %v", client.deletedKeys)
			}
		})
	}
}

func TestHetznerAcquireNativeHTTPReadinessBinding(t *testing.T) {
	for _, observedID := range []int64{42, 43} {
		t.Run(strconv.FormatInt(observedID, 10), func(t *testing.T) {
			var mu sync.Mutex
			var requests []string
			var created Server
			var createdKey core.SSHKey
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				requests = append(requests, r.Method+" "+r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch r.Method + " " + r.URL.Path {
				case "GET /servers":
					_, _ = io.WriteString(w, `{"servers":[]}`)
				case "GET /ssh_keys":
					keys := []core.SSHKey{}
					if createdKey.ID != 0 {
						keys = append(keys, createdKey)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"ssh_keys": keys})
				case "POST /ssh_keys":
					if err := json.NewDecoder(r.Body).Decode(&createdKey); err != nil {
						t.Error(err)
						http.Error(w, "invalid key", 400)
						return
					}
					createdKey.ID = 7
					_ = json.NewEncoder(w).Encode(map[string]any{"ssh_key": createdKey})
				case "POST /servers":
					var input struct {
						Name   string
						Labels map[string]string
					}
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Error(err)
						http.Error(w, "invalid server", 400)
						return
					}
					created = Server{ID: 42, Name: input.Name, Labels: input.Labels}
					_ = json.NewEncoder(w).Encode(map[string]any{"server": created})
				case "GET /servers/42":
					ready := created
					ready.ID = observedID
					ready.PublicNet.IPv4.IP = "203.0.113.11"
					_ = json.NewEncoder(w).Encode(map[string]any{"server": ready})
				case "PUT /servers/42", "DELETE /servers/42", "DELETE /ssh_keys/7":
					_, _ = io.WriteString(w, `{}`)
				default:
					t.Errorf("unexpected native request %s %s", r.Method, r.URL.Path)
					http.Error(w, "unexpected request", 400)
				}
			}))
			defer api.Close()
			installHetznerTestHooks(t, &fakeHetznerClient{})
			client := &core.HetznerClient{Token: "synthetic", Client: api.Client(), BaseURL: api.URL}
			newHetznerClient = func() (hetznerClient, error) { return client, nil }
			waitForServerIP = func(ctx context.Context, _ hetznerClient, id int64) (Server, error) {
				return core.WaitForServerIP(ctx, client, id)
			}
			sshCalls := 0
			waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
				sshCalls++
				if observedID != 42 {
					return errors.New("unexpected readiness SSH")
				}
				return nil
			}
			cfg := Config{ServerType: "cpx11", ServerTypeExplicit: true, Location: "test", Image: "ubuntu-24.04"}
			backend := NewHetznerLeaseBackend(ProviderSpec{}, cfg, Runtime{Stderr: io.Discard}).(*hetznerLeaseBackend)
			lease, err := backend.Acquire(context.Background(), AcquireRequest{RequestedSlug: "test"})
			mu.Lock()
			trace := strings.Join(requests, "\n")
			mu.Unlock()
			if observedID == 42 {
				if err != nil || lease.Server.ID != 42 || sshCalls != 1 || !strings.Contains(trace, "PUT /servers/42") || strings.Contains(trace, "DELETE") {
					t.Fatalf("lease=%+v err=%v SSH=%d requests:\n%s", lease, err, sshCalls, trace)
				}
			} else if err == nil || !strings.Contains(err.Error(), "readiness rejected") || sshCalls != 0 || !strings.HasSuffix(trace, "DELETE /ssh_keys/7\nDELETE /servers/42") || strings.Contains(trace, "PUT") || strings.Contains(trace, "/servers/43") {
				t.Fatalf("err=%v SSH=%d requests:\n%s", err, sshCalls, trace)
			}
			t.Logf("created=42 observed=%d SSH=%d err=%v\nnative HTTP:\n%s", observedID, sshCalls, err, trace)
		})
	}
}

func TestHetznerAcquireReportsRollbackFailure(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	server := acquiredHetznerTestServer(42, leaseID)
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
	_, err := backend.acquireOnce(context.Background(), false, "test")
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
		CloudID: strconv.FormatInt(id, 10),
		ID:      id,
		Name:    "crabbox-test",
		Labels:  map[string]string{"crabbox": "true", "created_by": "crabbox", "provider": providerName, "lease": leaseID, "slug": "test"},
	}
	server.PublicNet.IPv4.IP = "203.0.113.10"
	return server
}

func seedHetznerClaim(t *testing.T, server Server) {
	t.Helper()
	cfg := Config{Provider: providerName}
	if err := core.ClaimLeaseTargetForRepoConfig(server.Labels["lease"], server.Labels["slug"], cfg, normalizeHetznerServer(server), SSHTarget{}, "/repo", 30*time.Minute, false); err != nil {
		t.Fatalf("seed claim: %v", err)
	}
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
