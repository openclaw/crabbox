package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type deletingClaimResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
}

type stoppingClaimResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
}

type creatingMinimalClaimResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
	repo  string
}

type resolveResultBackend struct {
	testSSHBackend
	lease                  LeaseTarget
	rebindStoredTestboxKey bool
}

type providerManagedResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
}

type reclaimingStateLagResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
	repo  string
}

type recreatingClaimResolveBackend struct {
	testSSHBackend
	lease LeaseTarget
	repo  string
}

func TestRegisteredWebVNCDaemonCredentialInputUsesTransientConfig(t *testing.T) {
	const passwordEnv = "TEST_REGISTERED_ARD_PASSWORD"
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.TargetOS = targetMacOS
	cfg.External.Connection.Desktop.PasswordEnv = passwordEnv
	if err := setExternalDesktopTransientCredential(&cfg, passwordEnv, "operator-secret"); err != nil {
		t.Fatal(err)
	}

	args := []string{
		"--provider", "external",
		"--target", targetMacOS,
		"--external-desktop-password-env", passwordEnv,
		"--id", "cbx_abcdef123456",
	}
	credential := registeredWebVNCDaemonCredentialInput(cfg, args)
	if credential == nil || *credential != "operator-secret" {
		t.Fatalf("credential=%v args=%#v", credential, args)
	}
	if environment := strings.Join(webVNCDaemonChildEnvironment([]string{
		"PATH=/bin",
		passwordEnv + "=operator-secret",
	}, args, passwordEnv), "\n"); strings.Contains(environment, passwordEnv) || strings.Contains(environment, "operator-secret") {
		t.Fatalf("daemon environment retained desktop credential: %q", environment)
	}
	environmentCfg := cfg
	environmentCfg.externalDesktopCredentialName = ""
	environmentCfg.externalDesktopCredential = nil
	t.Setenv(passwordEnv, "environment-secret")
	credential = registeredWebVNCDaemonCredentialInput(environmentCfg, args)
	if credential == nil || *credential != "environment-secret" {
		t.Fatalf("environment credential=%v args=%#v", credential, args)
	}

	linuxArgs := append([]string(nil), args...)
	linuxArgs[3] = targetLinux
	if credential := registeredWebVNCDaemonCredentialInput(cfg, linuxArgs); credential != nil {
		t.Fatalf("non-macOS bridge received desktop credential: %q", *credential)
	}
}

func (b resolveResultBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}

func (b providerManagedResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	return b.lease, nil
}

func (b reclaimingStateLagResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	if err := claimLeaseForRepoProvider(b.lease.LeaseID, serverSlug(b.lease.Server), b.lease.Server.Provider, b.repo, time.Hour, true); err != nil {
		return LeaseTarget{}, err
	}
	return b.lease, nil
}

func (b recreatingClaimResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	removeLeaseClaim(b.lease.LeaseID)
	if err := claimLeaseForRepoProvider(b.lease.LeaseID, serverSlug(b.lease.Server), b.lease.Server.Provider, b.repo, time.Hour, true); err != nil {
		return LeaseTarget{}, err
	}
	return b.lease, nil
}

func (b resolveResultBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	if b.rebindStoredTestboxKey {
		useStoredTestboxKey(&target.SSH, leaseID)
	}
	return nil
}

func (b creatingMinimalClaimResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	if err := claimLeaseForRepoProvider(b.lease.LeaseID, serverSlug(b.lease.Server), b.lease.Server.Provider, b.repo, time.Hour, true); err != nil {
		return LeaseTarget{}, err
	}
	return b.lease, nil
}

func (b creatingMinimalClaimResolveBackend) RebindResolvedLeaseTarget(target *LeaseTarget, leaseID string) error {
	useStoredTestboxKey(&target.SSH, leaseID)
	return nil
}

func (b stoppingClaimResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	stopped := b.lease.Server
	stopped.Labels = cloneStringMap(stopped.Labels)
	stopped.Labels["state"] = "stopped"
	if err := updateLeaseClaimEndpoint(b.lease.LeaseID, stopped, SSHTarget{}); err != nil {
		return LeaseTarget{}, err
	}
	return b.lease, nil
}

func (b deletingClaimResolveBackend) Resolve(context.Context, ResolveRequest) (LeaseTarget, error) {
	removeLeaseClaim(b.lease.LeaseID)
	return b.lease, nil
}

func TestResolveSSHLeaseTargetMarksDeletedClaimRequired(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_deletedresolve123"
	server := Server{
		CloudID:  "i-deleted",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "deleted", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.40", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "deleted", cfg, server, target, "/repo", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	backend := deletingClaimResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), backend, ResolveRequest{ID: leaseID})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotSet || !lease.Server.claimSnapshotExists {
		t.Fatal("deleted pre-existing claim was treated as adoptable")
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "deleted", cfg, lease.Server, target, "/repo", true); err == nil {
		t.Fatal("deleted pre-existing claim was recreated")
	}
}

func TestResolveSSHLeaseTargetRejectsUnattestedClaimChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_changedresolve123"
	server := Server{
		CloudID:  "i-changed",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "changed", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.50", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "changed", cfg, server, target, "/repo", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	backend := stoppingClaimResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), backend, ResolveRequest{ID: leaseID, Repo: Repo{Root: "/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "changed", cfg, lease.Server, target, "/repo", true); err == nil {
		t.Fatal("unattested stopped claim was overwritten")
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels["state"] != "stopped" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestClaimAcquiredLeaseRejectsChangeAfterProviderSnapshot(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_COORDINATOR", "")
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_acquiresnapshot1"
	server := Server{
		CloudID:  "i-acquired",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "lease": leaseID, "slug": "acquired", "state": "ready"},
	}
	target := SSHTarget{Host: "192.0.2.60", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "acquired", cfg, server, target, "/repo", time.Hour, false); err != nil {
		t.Fatal(err)
	}
	acquired, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	server.claimSnapshot = acquired
	server.claimSnapshotExists = true
	server.claimSnapshotSet = true
	changedLabels := cloneStringMap(acquired.Labels)
	changedLabels["state"] = "reclaimed"
	if _, err := updateLeaseClaimLabelsIfUnchanged(leaseID, acquired, changedLabels); err != nil {
		t.Fatal(err)
	}

	err = (App{}).claimLeaseTargetForRepoAndRegister(context.Background(), leaseID, "acquired", cfg, server, target, "/repo", true)
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("err=%v, want acquisition-snapshot conflict", err)
	}
	current, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Labels["state"] != "reclaimed" {
		t.Fatalf("claim state=%q, want concurrent change preserved", current.Labels["state"])
	}
}

func TestResolveSSHLeaseTargetAcceptsMinimalProviderClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_minimalclaim123"
	server := Server{
		CloudID:  "sprite-minimal",
		Provider: "sprites",
		Labels:   map[string]string{"provider": "sprites", "slug": "minimal", "state": "ready"},
	}
	backend := creatingMinimalClaimResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "sprites"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: SSHTarget{Host: "192.0.2.60", Port: "22"}},
		repo:           "/repo",
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), backend, ResolveRequest{ID: "sprite-minimal", Repo: Repo{Root: "/repo"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotExists {
		t.Fatal("provider-created minimal claim was not accepted")
	}
}

func TestResolveSSHLeaseTargetFindsExistingClaimByCloudID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_cloudlookup123"
	server := Server{
		CloudID:  "i-cloudlookup",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "cloudlookup", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.70", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "cloudlookup", cfg, server, target, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("canonical-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	remoteServer := server
	remoteServer.Labels = cloneStringMap(server.Labels)
	remoteServer.Labels["lease"] = "cbx_remoteunknown"
	remoteServer.Labels["slug"] = "remote-unknown"
	lease, err := resolveSSHLeaseTarget(context.Background(), resolveResultBackend{
		testSSHBackend:         testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:                  LeaseTarget{LeaseID: "cbx_remoteunknown", Server: remoteServer, SSH: SSHTarget{Host: target.Host, Port: target.Port, Key: "/tmp/wrong-key"}},
		rebindStoredTestboxKey: true,
	}, ResolveRequest{ID: "i-cloudlookup", Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotExists {
		t.Fatal("cloud-id claim was not found before resolve")
	}
	if lease.LeaseID != leaseID {
		t.Fatalf("lease ID=%q want canonical %q", lease.LeaseID, leaseID)
	}
	if lease.SSH.Key != keyPath {
		t.Fatalf("ssh key=%q want canonical %q", lease.SSH.Key, keyPath)
	}
	if lease.Server.Labels["lease"] != leaseID || lease.Server.Labels["slug"] != "cloudlookup" {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "cloudlookup", cfg, lease.Server, target, "/repo-b", true); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/repo-b" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolvedLeaseClaimBeforePrefersExactLeaseID(t *testing.T) {
	exact := leaseClaim{LeaseID: "cbx_exact123456", Provider: "aws", Slug: "exact"}
	alias := leaseClaim{LeaseID: "cbx_other123456", Provider: "aws", Slug: "cbx_exact123456"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{alias, exact}},
		"aws",
		"",
		"cbx_exact123456",
		LeaseTarget{LeaseID: "cbx_exact123456"},
	)
	if err != nil || !ok || claim.LeaseID != exact.LeaseID {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvedLeaseClaimBeforeMatchesProviderScope(t *testing.T) {
	projectA := leaseClaim{LeaseID: "cbx_projecta123", Provider: "gcp", ProviderScope: "project:project-a", CloudID: "vm-shared", Slug: "shared"}
	projectB := leaseClaim{LeaseID: "cbx_projectb123", Provider: "gcp", ProviderScope: "project:project-b", CloudID: "vm-shared", Slug: "shared"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{projectA, projectB}},
		"gcp",
		"project:project-b",
		"shared",
		LeaseTarget{LeaseID: "cbx_remote123456", Server: Server{CloudID: "vm-shared", Provider: "gcp"}},
	)
	if err != nil || !ok || claim.LeaseID != projectB.LeaseID {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvedLeaseClaimBeforeRejectsExactIdentifierFromOtherScope(t *testing.T) {
	other := leaseClaim{LeaseID: "cbx_scoped123456", Provider: "gcp", ProviderScope: "project:project-a", CloudID: "vm-a", Slug: "scoped"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{other}},
		"gcp",
		"project:project-b",
		other.LeaseID,
		LeaseTarget{LeaseID: "cbx_remote123456", Server: Server{CloudID: "vm-b", Provider: "gcp"}},
	)
	if err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvedLeaseClaimBeforeAcceptsExactIdentifierWithDynamicScope(t *testing.T) {
	scoped := leaseClaim{LeaseID: "cbx_dynamic123456", Provider: "local-container", ProviderScope: "runtime:docker/context:desktop", CloudID: "container-a"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{scoped}},
		"local-container",
		"",
		scoped.LeaseID,
		LeaseTarget{LeaseID: scoped.LeaseID, Server: Server{CloudID: scoped.CloudID, Provider: "local-container"}},
	)
	if err != nil || !ok || claim.LeaseID != scoped.LeaseID {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvedLeaseClaimBeforeRejectsSlugWithDifferentCloudID(t *testing.T) {
	stale := leaseClaim{LeaseID: "cbx_staleslug123", Provider: "aws", CloudID: "i-old", Slug: "shared"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{stale}},
		"aws",
		"",
		"shared",
		LeaseTarget{LeaseID: "cbx_remote123456", Server: Server{CloudID: "i-new", Provider: "aws"}},
	)
	if err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolveSSHLeaseTargetRemovesProviderCreatedAliasClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_canonical123456"
	aliasID := "cbx_remote123456"
	server := Server{
		CloudID:  "i-alias-created",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "lease": aliasID, "slug": "remote-alias", "state": "running"},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "canonical", cfg, server, SSHTarget{}, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	canonicalKeyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	aliasKeyPath, err := testboxKeyPath(aliasID)
	if err != nil {
		t.Fatal(err)
	}
	for _, keyPath := range []string{canonicalKeyPath, aliasKeyPath} {
		if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyPath, []byte("test-key"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), creatingMinimalClaimResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: aliasID, Server: server, SSH: SSHTarget{Host: "192.0.2.110", Port: "22", Key: aliasKeyPath}},
		repo:           "/repo-b",
	}, ResolveRequest{ID: "i-alias-created", Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID {
		t.Fatalf("lease ID=%q want %q", lease.LeaseID, leaseID)
	}
	if _, exists, err := readLeaseClaimWithPresence(aliasID); err != nil || exists {
		t.Fatalf("provider-created alias claim remains: exists=%v err=%v", exists, err)
	}
	if _, err := os.Stat(aliasKeyPath); !os.IsNotExist(err) {
		t.Fatalf("provider-created alias key remains: %v", err)
	}
	if lease.SSH.Key != canonicalKeyPath {
		t.Fatalf("ssh key=%q want %q", lease.SSH.Key, canonicalKeyPath)
	}
}

func TestResolveSSHLeaseTargetPreservesProviderManagedCredentials(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "tenki"
	leaseID := "cbx_tenki123456"
	server := Server{
		CloudID:  "session-123",
		Provider: "tenki",
		Labels:   map[string]string{"provider": "tenki", "slug": "tenki-session", "state": "running"},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "tenki-session", cfg, server, SSHTarget{}, "/repo", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("unrelated-stored-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	managed := SSHTarget{
		Host:            "sandbox",
		Port:            "22",
		Key:             "/tmp/tenki/identity",
		CertificateFile: "/tmp/tenki/certificate",
		SSHConfigProxy:  true,
		ProxyCommand:    "tenki sandbox ssh-proxy --session session-123",
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), providerManagedResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "tenki"}},
		lease:          LeaseTarget{LeaseID: "cbx_remote123456", Server: server, SSH: managed},
	}, ResolveRequest{ID: "session-123", Repo: Repo{Root: "/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != leaseID {
		t.Fatalf("lease ID=%q want %q", lease.LeaseID, leaseID)
	}
	if lease.SSH.Key != managed.Key || lease.SSH.CertificateFile != managed.CertificateFile || lease.SSH.ProxyCommand != managed.ProxyCommand {
		t.Fatalf("ssh=%#v", lease.SSH)
	}
}

func TestCoordinatorLeaseBackendForwardsResolvedTargetRebinding(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	leaseID := "cbx_coordinator123"
	keyPath, err := testboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, []byte("canonical-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &coordinatorLeaseBackend{
		direct: resolveResultBackend{
			testSSHBackend:         testSSHBackend{spec: ProviderSpec{Name: "aws"}},
			rebindStoredTestboxKey: true,
		},
	}
	target := LeaseTarget{SSH: SSHTarget{Key: "/tmp/alias-key"}}
	if err := backend.RebindResolvedLeaseTarget(&target, leaseID); err != nil {
		t.Fatal(err)
	}
	if target.SSH.Key != keyPath {
		t.Fatalf("ssh key=%q want %q", target.SSH.Key, keyPath)
	}
}

func TestResolvedLeaseClaimBeforeDoesNotMatchProviderlessClaimByReturnedCloudID(t *testing.T) {
	legacy := leaseClaim{LeaseID: "cbx_legacy123456", CloudID: "123", Slug: "legacy"}
	claim, ok, err := resolvedLeaseClaimBefore(
		leaseClaimsSnapshot{claims: []leaseClaim{legacy}},
		"hostinger",
		"",
		"123",
		LeaseTarget{LeaseID: "cbx_remote123456", Server: Server{CloudID: "123", Provider: "hostinger"}},
	)
	if err != nil || ok || claim.LeaseID != "" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolveSSHLeaseTargetRejectsRecreatedPreexistingClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "sprites"
	leaseID := "cbx_recreated123456"
	server := Server{
		CloudID:  "sprite-recreated",
		Provider: "sprites",
		Labels:   map[string]string{"provider": "sprites", "slug": "recreated", "state": "ready"},
	}
	target := SSHTarget{Host: "192.0.2.120", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "recreated", cfg, server, target, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), recreatingClaimResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "sprites"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
		repo:           "/repo-b",
	}, ResolveRequest{ID: leaseID, Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "recreated", cfg, lease.Server, target, "/repo-b", true); err == nil {
		t.Fatal("recreated claim replaced the pre-resolve claim generation")
	}
}

func TestResolveSSHLeaseTargetAcceptsReadyClaimForLeasedProviderState(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "local-container"
	leaseID := "cbx_statelag123456"
	ready := Server{
		CloudID:  "container-123",
		Provider: "local-container",
		Labels:   map[string]string{"provider": "local-container", "lease": leaseID, "slug": "state-lag", "state": "ready"},
	}
	target := SSHTarget{Host: "127.0.0.1", Port: "2222"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "state-lag", cfg, ready, target, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	leased := ready
	leased.Labels = cloneStringMap(ready.Labels)
	leased.Labels["state"] = "leased"
	lease, err := resolveSSHLeaseTarget(context.Background(), reclaimingStateLagResolveBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "local-container"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: leased, SSH: target},
		repo:           "/repo-b",
	}, ResolveRequest{ID: leaseID, Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotExists || lease.Server.claimSnapshot.RepoRoot != "/repo-b" {
		t.Fatalf("snapshot=%#v", lease.Server.claimSnapshot)
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "state-lag", cfg, lease.Server, target, "/repo-b", true); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSSHLeaseTargetReclaimsProviderlessLegacyClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_legacy123456"
	server := Server{
		CloudID:  "i-legacy",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "legacy", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.100", Port: "22"}
	path, err := leaseClaimPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := leaseClaim{
		LeaseID:   leaseID,
		Slug:      "legacy",
		CloudID:   "i-legacy",
		RepoRoot:  "/repo-a",
		ClaimedAt: time.Now().UTC().Format(time.RFC3339),
		Labels:    map[string]string{"slug": "legacy", "state": "running"},
	}
	if err := writeLeaseClaimAtomic(path, legacy); err != nil {
		t.Fatal(err)
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), resolveResultBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
	}, ResolveRequest{ID: leaseID, Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "legacy", cfg, lease.Server, target, "/repo-b", true); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Provider != "aws" || claim.RepoRoot != "/repo-b" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolveSSHLeaseTargetFindsExistingClaimByReturnedLeaseID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_aliaslookup123"
	server := Server{
		CloudID:  "i-aliaslookup",
		Provider: "aws",
		Name:     "provider-server-name",
		Labels:   map[string]string{"provider": "aws", "slug": "aliaslookup", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.80", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "aliaslookup", cfg, server, target, "/repo-a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), resolveResultBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
	}, ResolveRequest{ID: "provider-server-name", Repo: Repo{Root: "/repo-b"}, Reclaim: true})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotExists {
		t.Fatal("returned lease ID did not recover the pre-resolve claim")
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "aliaslookup", cfg, lease.Server, target, "/repo-b", true); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RepoRoot != "/repo-b" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolveSSHLeaseTargetIgnoresUnrelatedCorruptClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_validclaim123"
	server := Server{
		CloudID:  "i-validclaim",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "validclaim", "state": "running"},
	}
	target := SSHTarget{Host: "192.0.2.90", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "validclaim", cfg, server, target, "/repo", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	corruptPath, err := leaseClaimPath("cbx_unrelatedcorrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(corruptPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	lease, err := resolveSSHLeaseTarget(context.Background(), resolveResultBackend{
		testSSHBackend: testSSHBackend{spec: ProviderSpec{Name: "aws"}},
		lease:          LeaseTarget{LeaseID: leaseID, Server: server, SSH: target},
	}, ResolveRequest{ID: leaseID, Repo: Repo{Root: "/repo"}})
	if err != nil {
		t.Fatal(err)
	}
	if !lease.Server.claimSnapshotExists {
		t.Fatal("valid claim was not snapshotted")
	}
}

func TestResolvedLeaseClaimUpdatesRejectStoppedClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.IdleTimeout = time.Hour
	leaseID := "cbx_guarded123456"
	running := Server{
		CloudID:  "i-guarded",
		Provider: "aws",
		Labels: map[string]string{
			"provider": "aws",
			"slug":     "guarded",
			"state":    "running",
		},
	}
	target := SSHTarget{Host: "192.0.2.10", Port: "22"}
	if err := claimLeaseTargetForRepoConfig(leaseID, "guarded", cfg, running, target, "/repo/a", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	var err error
	running.claimSnapshot, running.claimSnapshotExists, err = readLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	running.claimSnapshotSet = true
	stopped := running
	stopped.Labels = cloneStringMap(running.Labels)
	stopped.Labels["state"] = "stopped"
	if err := updateLeaseClaimEndpoint(leaseID, stopped, SSHTarget{}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := updateResolvedLeaseClaimEndpoint(leaseID, running, target); err == nil {
		t.Fatal("stale endpoint update replaced stopped claim")
	}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "guarded", cfg, running, target, "/repo/b", true); err == nil {
		t.Fatal("stale claim update replaced stopped claim")
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, "aws")
	if err != nil || !ok {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
	if claim.Labels["state"] != "stopped" || claim.RepoRoot != "/repo/a" || claim.SSHHost != "" {
		t.Fatalf("stopped claim changed: %#v", claim)
	}

	removeLeaseClaim(leaseID)
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "guarded", cfg, running, target, "/repo/b", true); err == nil {
		t.Fatal("stale claim update recreated deleted claim")
	}
	if _, exists, err := readLeaseClaimWithPresence(leaseID); err != nil || exists {
		t.Fatalf("deleted claim recreated: exists=%v err=%v", exists, err)
	}
}

func TestResolvedLeaseClaimAllowsUnclaimedResourceAdoption(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.IdleTimeout = time.Hour
	leaseID := "cbx_adopt123456"
	server := Server{
		CloudID:  "i-adopt",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "slug": "adopt", "state": "running"},
	}
	server.claimSnapshotSet = true
	target := SSHTarget{Host: "192.0.2.30", Port: "22"}
	if err := (App{}).claimResolvedLeaseTargetForRepoAndRegister(context.Background(), leaseID, "adopt", cfg, server, target, "/repo", true); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, "aws")
	if err != nil || !ok || claim.RepoRoot != "/repo" || claim.CloudID != "i-adopt" {
		t.Fatalf("claim=%#v ok=%v err=%v", claim, ok, err)
	}
}

func TestResolvedLeaseClaimUpdatesRejectActiveStateChange(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := baseConfig()
	cfg.Provider = "aws"
	leaseID := "cbx_activechange123"
	running := Server{
		CloudID:  "vm-active",
		Provider: "aws",
		Labels:   map[string]string{"provider": "aws", "state": "running"},
	}
	if err := claimLeaseTargetForRepoConfig(leaseID, "active", cfg, running, SSHTarget{Host: "192.0.2.20", Port: "22"}, "/repo", time.Hour, true); err != nil {
		t.Fatal(err)
	}
	var err error
	running.claimSnapshot, running.claimSnapshotExists, err = readLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	running.claimSnapshotSet = true
	provisioning := running
	provisioning.Labels = cloneStringMap(running.Labels)
	provisioning.Labels["state"] = "provisioning"
	if err := updateLeaseClaimEndpoint(leaseID, provisioning, SSHTarget{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := updateResolvedLeaseClaimEndpoint(leaseID, running, SSHTarget{Host: "192.0.2.20", Port: "22"}); err == nil {
		t.Fatal("stale running endpoint replaced provisioning claim")
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels["state"] != "provisioning" {
		t.Fatalf("provisioning claim changed: %#v", claim)
	}
}

func TestRegisterCoordinatorLeaseBestEffortMapsDirectLease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	var got CoordinatorLeaseRegistration
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": map[string]any{
			"id": "cbx_123", "provider": "external", "lifecycle": "registered", "state": "active",
			"runtimeAdapterID": "mac-lab", "runtimeAdapterWorkspaceID": "fleet-a-is-123",
			"runtimeAdapterRegistrationID": got.RuntimeRegistrationID,
		}})
	}))
	defer server.Close()

	var stderr bytes.Buffer
	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "token"
	cfg.BrokerMode = BrokerModeRegistered
	cfg.Desktop = true
	cfg.DesktopEnv = "gnome"
	cfg.WorkRoot = "/workspace"
	cfg.ExposedPorts = []string{"3000", "8080"}
	cfg.TTL = 2 * time.Hour
	cfg.IdleTimeout = 30 * time.Minute
	app := App{Stderr: &stderr}
	lease := LeaseTarget{
		LeaseID: "cbx_123",
		Server: Server{
			Provider: "external",
			CloudID:  "external-box-123",
			Name:     "my-box",
		},
		SSH: SSHTarget{Host: "192.0.2.10", User: "runner", Port: "22", TargetOS: targetLinux},
	}
	lease.Server.ServerType.Name = "cpu16"
	if err := claimLeaseTargetForRepoConfig(
		lease.LeaseID, "my-box", cfg, lease.Server, lease.SSH, "/workspace", cfg.IdleTimeout, true,
	); err != nil {
		t.Fatal(err)
	}
	app.registerCoordinatorLeaseBestEffort(context.Background(), cfg, lease)
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got.Provider != "external" || got.CloudID != "external-box-123" || got.Host != "192.0.2.10" || got.WorkRoot != "/workspace" || !got.Desktop || got.DesktopEnv != "gnome" || len(got.ExposedPorts) != 2 || got.TTLSeconds != 7200 || got.IdleTimeoutSeconds != 1800 || got.RuntimeAdapterID != "mac-lab" || got.RuntimeWorkspaceID != "fleet-a-is-123" || got.RuntimeRegistrationID == "" {
		t.Fatalf("registration=%#v", got)
	}
	claim, err := readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RuntimeAdapterRegistrationID != got.RuntimeRegistrationID {
		t.Fatalf("claim registration id=%q request=%q", claim.RuntimeAdapterRegistrationID, got.RuntimeRegistrationID)
	}
}

func TestAdapterRegistrationRotatesRejectedTerminalGeneration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	var registrationIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got CoordinatorLeaseRegistration
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		registrationIDs = append(registrationIDs, got.RuntimeRegistrationID)
		w.Header().Set("Content-Type", "application/json")
		if len(registrationIDs) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"runtime_adapter_registration_replayed"}`))
			return
		}
		if len(registrationIDs) == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"temporarily_unavailable"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"lease": map[string]any{
			"id": "cbx_123", "provider": "external", "lifecycle": "registered", "state": "active",
			"runtimeAdapterID": "mac-lab", "runtimeAdapterWorkspaceID": "fleet-a-is-123",
			"runtimeAdapterRegistrationID": got.RuntimeRegistrationID,
		}})
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "external"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "token"
	cfg.BrokerMode = BrokerModeRegistered
	cfg.IdleTimeout = time.Hour
	lease := LeaseTarget{
		LeaseID: "cbx_123",
		Server:  Server{Provider: "external", CloudID: "external-box-123"},
		SSH:     SSHTarget{Host: "192.0.2.10", Port: "22", TargetOS: targetLinux},
	}
	if err := claimLeaseTargetForRepoConfig(
		lease.LeaseID, "adapter-box", cfg, lease.Server, lease.SSH, "/repo", cfg.IdleTimeout, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim(lease.LeaseID, func(claim *leaseClaim) error {
		claim.RuntimeAdapterRegistrationID = "registration-generation-old"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	app := App{Stderr: &stderr}
	if err := app.registerCoordinatorLeaseBestEffort(
		context.Background(), cfg, lease,
	); err == nil {
		t.Fatal("replacement registration unexpectedly succeeded through a transport failure")
	}
	claim, err := readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrationIDs) != 2 ||
		registrationIDs[0] != "registration-generation-old" ||
		registrationIDs[1] == "" ||
		registrationIDs[1] == registrationIDs[0] {
		t.Fatalf("registration ids=%q", registrationIDs)
	}
	if claim.RuntimeAdapterRegistrationID != registrationIDs[0] ||
		claim.RuntimeAdapterPendingRegistrationID != registrationIDs[1] {
		t.Fatalf("failed replacement claim=%#v requests=%q", claim, registrationIDs)
	}
	stderr.Reset()
	if err := app.registerCoordinatorLeaseBestEffort(context.Background(), cfg, lease); err != nil {
		t.Fatal(err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if len(registrationIDs) != 3 || registrationIDs[2] != registrationIDs[1] {
		t.Fatalf("retried registration ids=%q", registrationIDs)
	}
	claim, err = readLeaseClaim(lease.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.RuntimeAdapterRegistrationID != registrationIDs[1] ||
		claim.RuntimeAdapterPendingRegistrationID != "" {
		t.Fatalf("claim registration id=%q requests=%q", claim.RuntimeAdapterRegistrationID, registrationIDs)
	}
}

func TestAdapterClaimRequiresExactCoordinatorRuntimeBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_adapter123","provider":"aws","lifecycle":"registered","state":"active","runtimeAdapterID":"other-lab","runtimeAdapterWorkspaceID":"fleet-a-is-123"}}`))
	}))
	defer server.Close()

	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.Coordinator = server.URL
	cfg.CoordToken = "token"
	cfg.BrokerMode = BrokerModeRegistered
	cfg.IdleTimeout = time.Hour
	leaseID := "cbx_adapter123"
	leaseServer := Server{
		Provider: "aws",
		CloudID:  "i-adapter",
		Labels:   map[string]string{"provider": "aws", "slug": "adapter", "state": "running"},
	}
	var stderr bytes.Buffer
	err := (App{Stderr: &stderr}).claimLeaseTargetForRepoAndRegister(
		context.Background(),
		leaseID,
		"adapter",
		cfg,
		leaseServer,
		SSHTarget{Host: "192.0.2.42", User: "runner", Port: "22"},
		"/repo",
		true,
	)
	if err == nil || !strings.Contains(err.Error(), "expected \"mac-lab\"/\"fleet-a-is-123\"") {
		t.Fatalf("registration error=%v stderr=%q", err, stderr.String())
	}
}

func TestControllerClaimWithoutAdapterIDDoesNotRequireCoordinatorBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "")
	t.Setenv(controllerWorkspaceIDEnv, "local-workspace")
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.IdleTimeout = time.Hour
	leaseID := "cbx_localadapter123"
	server := Server{
		Provider: "aws",
		CloudID:  "i-local-adapter",
		Labels:   map[string]string{"provider": "aws", "slug": "local-adapter", "state": "running"},
	}
	if err := (App{}).claimLeaseTargetForRepoAndRegister(
		context.Background(), leaseID, "local-adapter", cfg, server, SSHTarget{}, "/repo", true,
	); err != nil {
		t.Fatal(err)
	}
}

func TestAmbientAdapterIDDoesNotRequireControllerCoordinatorBinding(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "")
	cfg := baseConfig()
	cfg.Provider = "aws"
	cfg.IdleTimeout = time.Hour
	leaseID := "cbx_ambientadapter123"
	server := Server{
		Provider: "aws",
		CloudID:  "i-ambient-adapter",
		Labels:   map[string]string{"provider": "aws", "slug": "ambient-adapter", "state": "running"},
	}
	if err := (App{}).claimLeaseTargetForRepoAndRegister(
		context.Background(), leaseID, "ambient-adapter", cfg, server, SSHTarget{}, "/repo", true,
	); err != nil {
		t.Fatal(err)
	}
}

func TestReleaseRegisteredCoordinatorLeaseNeverRequestsProviderDeletion(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var got struct {
		Delete bool `json:"delete"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"external","lifecycle":"registered","state":"released"}}`))
	}))
	defer server.Close()

	var stderr bytes.Buffer
	app := App{Stdout: &bytes.Buffer{}, Stderr: &stderr}
	app.releaseRegisteredCoordinatorLeaseBestEffort(context.Background(), Config{
		Coordinator: server.URL,
		CoordToken:  "token",
		BrokerMode:  BrokerModeRegistered,
	}, "cbx_123")
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if got.Delete {
		t.Fatal("registered coordinator release requested provider deletion")
	}
}

func TestControllerManagedCoordinatorDeregistrationWaitsForConfirmedAbsence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	requests := 0
	var completion struct {
		RuntimeAdapterDeleteCompletion struct {
			AdapterID      string `json:"adapterID"`
			WorkspaceID    string `json:"workspaceID"`
			RegistrationID string `json:"registrationID"`
			Status         string `json:"status"`
		} `json:"runtimeAdapterDeleteCompletion"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&completion); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","provider":"external","lifecycle":"registered","state":"released"}}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := claimLeaseTargetForRepoConfig(
		"cbx_123", "adapter-box", cfg,
		Server{Provider: "external", CloudID: "adapter-box"},
		SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim("cbx_123", func(claim *leaseClaim) error {
		claim.RuntimeAdapterRegistrationID = "registration-generation-123"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app.releaseRegisteredCoordinatorLeaseBestEffort(context.Background(), cfg, "cbx_123")
	if requests != 0 {
		t.Fatalf("controller-owned provider stop deregistered coordinator early: requests=%d", requests)
	}
	if err := app.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(context.Background(), cfg, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Fatalf("confirmed absence deregistration requests=%d", requests)
	}
	if completion.RuntimeAdapterDeleteCompletion.AdapterID != "mac-lab" ||
		completion.RuntimeAdapterDeleteCompletion.WorkspaceID != "fleet-a-is-123" ||
		completion.RuntimeAdapterDeleteCompletion.RegistrationID != "registration-generation-123" ||
		completion.RuntimeAdapterDeleteCompletion.Status != "absent" {
		t.Fatalf("confirmed absence completion=%#v", completion.RuntimeAdapterDeleteCompletion)
	}
}

func TestConfirmedAbsenceFallsBackFromPendingToAcknowledgedGeneration(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	var registrationIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var completion struct {
			RuntimeAdapterDeleteCompletion struct {
				RegistrationID string `json:"registrationID"`
			} `json:"runtimeAdapterDeleteCompletion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&completion); err != nil {
			t.Fatal(err)
		}
		registrationIDs = append(registrationIDs, completion.RuntimeAdapterDeleteCompletion.RegistrationID)
		w.Header().Set("Content-Type", "application/json")
		if len(registrationIDs) == 1 {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_completion_mismatch"}`))
			return
		}
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","state":"released"}}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	if err := claimLeaseTargetForRepoConfig(
		"cbx_123", "adapter-box", cfg,
		Server{Provider: "external", CloudID: "adapter-box"},
		SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim("cbx_123", func(claim *leaseClaim) error {
		claim.RuntimeAdapterRegistrationID = "registration-generation-current"
		claim.RuntimeAdapterPendingRegistrationID = "registration-generation-pending"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(context.Background(), cfg, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registrationIDs, []string{
		"registration-generation-pending",
		"registration-generation-current",
	}) {
		t.Fatalf("completion registration ids=%q", registrationIDs)
	}
}

func TestConfirmedAbsenceFallsBackToVerifiedLegacyReleaseAfterGenerationMismatches(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	var requests []string
	var registrationIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_123/release":
			var body struct {
				RuntimeAdapterDeleteCompletion *struct {
					RegistrationID string `json:"registrationID"`
				} `json:"runtimeAdapterDeleteCompletion"`
				RuntimeAdapterLegacyDeleteCompletion *struct {
					AdapterID   string `json:"adapterID"`
					WorkspaceID string `json:"workspaceID"`
					Status      string `json:"status"`
				} `json:"runtimeAdapterLegacyDeleteCompletion"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.RuntimeAdapterDeleteCompletion != nil {
				requests = append(requests, "complete")
				registrationIDs = append(registrationIDs, body.RuntimeAdapterDeleteCompletion.RegistrationID)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_completion_mismatch"}`))
				return
			}
			if body.RuntimeAdapterLegacyDeleteCompletion == nil {
				t.Fatal("missing atomic legacy runtime adapter completion")
			}
			requests = append(requests, "legacy-complete")
			if completion := body.RuntimeAdapterLegacyDeleteCompletion; completion.AdapterID != "mac-lab" ||
				completion.WorkspaceID != "fleet-a-is-123" || completion.Status != "absent" {
				t.Fatalf("legacy completion=%#v", completion)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","state":"released"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	if err := claimLeaseTargetForRepoConfig(
		"cbx_123", "adapter-box", cfg,
		Server{Provider: "external", CloudID: "adapter-box"},
		SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim("cbx_123", func(claim *leaseClaim) error {
		claim.RuntimeAdapterRegistrationID = "registration-generation-current"
		claim.RuntimeAdapterPendingRegistrationID = "registration-generation-pending"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(context.Background(), cfg, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(registrationIDs, []string{
		"registration-generation-pending",
		"registration-generation-current",
	}) {
		t.Fatalf("completion registration ids=%q", registrationIDs)
	}
	if !reflect.DeepEqual(requests, []string{"complete", "complete", "legacy-complete"}) {
		t.Fatalf("requests=%q", requests)
	}
}

func TestConfirmedAbsenceGenerationMismatchRejectsGenerationAwareLegacyFallback(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	completionRequests := 0
	legacyCompletionRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/leases/cbx_123/release":
			var body struct {
				RuntimeAdapterDeleteCompletion       json.RawMessage `json:"runtimeAdapterDeleteCompletion"`
				RuntimeAdapterLegacyDeleteCompletion *struct {
					AdapterID   string `json:"adapterID"`
					WorkspaceID string `json:"workspaceID"`
					Status      string `json:"status"`
				} `json:"runtimeAdapterLegacyDeleteCompletion"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			if len(body.RuntimeAdapterDeleteCompletion) != 0 {
				completionRequests++
				_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_completion_mismatch"}`))
				return
			}
			if completion := body.RuntimeAdapterLegacyDeleteCompletion; completion == nil ||
				completion.AdapterID != "mac-lab" || completion.WorkspaceID != "fleet-a-is-123" ||
				completion.Status != "absent" {
				t.Fatalf("legacy completion=%#v", completion)
			}
			legacyCompletionRequests++
			_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_completion_mismatch"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	if err := claimLeaseTargetForRepoConfig(
		"cbx_123", "adapter-box", cfg,
		Server{Provider: "external", CloudID: "adapter-box"},
		SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
	); err != nil {
		t.Fatal(err)
	}
	if err := mutateLeaseClaim("cbx_123", func(claim *leaseClaim) error {
		claim.RuntimeAdapterRegistrationID = "registration-generation-old"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	err := (App{}).releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(
		context.Background(),
		cfg,
		"cbx_123",
	)
	if err == nil || !strings.Contains(err.Error(), "runtime_adapter_delete_completion_mismatch") {
		t.Fatalf("generation-aware cleanup error=%v", err)
	}
	if completionRequests != 1 || legacyCompletionRequests != 1 {
		t.Fatalf("completion=%d legacy completion=%d", completionRequests, legacyCompletionRequests)
	}
}

func TestControllerManagedCoordinatorDeregistrationWithoutGenerationUsesLegacyRelease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	requests := 0
	var completion struct {
		AdapterID   string `json:"adapterID"`
		WorkspaceID string `json:"workspaceID"`
		Status      string `json:"status"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RuntimeAdapterLegacyDeleteCompletion json.RawMessage `json:"runtimeAdapterLegacyDeleteCompletion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body.RuntimeAdapterLegacyDeleteCompletion, &completion); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_123","state":"released"}}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	if err := claimLeaseTargetForRepoConfig(
		"cbx_123", "adapter-box", cfg,
		Server{Provider: "external", CloudID: "adapter-box"},
		SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
	); err != nil {
		t.Fatal(err)
	}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(context.Background(), cfg, "cbx_123"); err != nil {
		t.Fatal(err)
	}
	if requests != 1 || completion.AdapterID != "mac-lab" || completion.WorkspaceID != "fleet-a-is-123" ||
		completion.Status != "absent" {
		t.Fatalf("requests=%d completion=%#v", requests, completion)
	}
}

func TestExpiredLegacyPendingDeleteUsesAtomicCoordinatorFinalizer(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	pending := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_expired/release" {
			t.Fatalf("expired pending cleanup used non-atomic request %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			RuntimeAdapterLegacyDeleteCompletion *struct {
				AdapterID   string `json:"adapterID"`
				WorkspaceID string `json:"workspaceID"`
				Status      string `json:"status"`
			} `json:"runtimeAdapterLegacyDeleteCompletion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if completion := body.RuntimeAdapterLegacyDeleteCompletion; completion == nil ||
			completion.AdapterID != "mac-lab" || completion.WorkspaceID != "fleet-a-is-123" ||
			completion.Status != "absent" {
			t.Fatalf("legacy completion=%#v", completion)
		}
		pending = false
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"lease":{"id":"cbx_expired","state":"released"}}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	if err := (App{}).releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(
		context.Background(),
		cfg,
		"cbx_expired",
	); err != nil {
		t.Fatal(err)
	}
	if pending {
		t.Fatal("expired legacy pending cleanup was treated as already complete")
	}
}

func TestControllerManagedCoordinatorDeregistrationWithoutClaimRejectsGenerationAwareRelease(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	completionRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		completionRequests++
		var body struct {
			RuntimeAdapterLegacyDeleteCompletion json.RawMessage `json:"runtimeAdapterLegacyDeleteCompletion"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body.RuntimeAdapterLegacyDeleteCompletion) == 0 {
			t.Fatal("missing atomic legacy runtime adapter completion")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"runtime_adapter_delete_completion_mismatch"}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	err := (App{}).releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(
		context.Background(),
		cfg,
		"cbx_123",
	)
	if err == nil || !strings.Contains(err.Error(), "runtime_adapter_delete_completion_mismatch") {
		t.Fatalf("generation-aware cleanup error=%v", err)
	}
	if completionRequests != 1 {
		t.Fatalf("generation-aware atomic completion requests=%d", completionRequests)
	}
}

func TestConfirmedAbsenceWithoutAdapterBindingRejectsPersistedGeneration(t *testing.T) {
	for _, field := range []string{"current", "pending"} {
		t.Run(field, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			t.Setenv("CRABBOX_ADAPTER_ID", "")
			t.Setenv(controllerWorkspaceIDEnv, "")
			requests := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				t.Fatalf("generation-aware cleanup made unexpected request %s %s", r.Method, r.URL.Path)
			}))
			defer server.Close()

			cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
			if err := claimLeaseTargetForRepoConfig(
				"cbx_123", "adapter-box", cfg,
				Server{Provider: "external", CloudID: "adapter-box"},
				SSHTarget{Host: "192.0.2.10", Port: "22"}, "/repo", time.Hour, true,
			); err != nil {
				t.Fatal(err)
			}
			if err := mutateLeaseClaim("cbx_123", func(claim *leaseClaim) error {
				if field == "current" {
					claim.RuntimeAdapterRegistrationID = "registration-generation-current"
				} else {
					claim.RuntimeAdapterPendingRegistrationID = "registration-generation-pending"
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			err := (App{}).releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(
				context.Background(),
				cfg,
				"cbx_123",
			)
			if err == nil || !strings.Contains(err.Error(), "requires adapter binding") {
				t.Fatalf("generation-aware cleanup error=%v", err)
			}
			if requests != 0 {
				t.Fatalf("generation-aware cleanup requests=%d", requests)
			}
		})
	}
}

func TestConfirmedAbsenceCoordinatorDeregistrationTreatsMissingAsClean(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CRABBOX_ADAPTER_ID", "mac-lab")
	t.Setenv(controllerWorkspaceIDEnv, "fleet-a-is-123")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/leases/cbx_123/release" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not_found"}`))
	}))
	defer server.Close()

	cfg := Config{Coordinator: server.URL, CoordToken: "token", BrokerMode: BrokerModeRegistered}
	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := app.releaseRegisteredCoordinatorLeaseAfterConfirmedAbsence(context.Background(), cfg, "cbx_123"); err != nil {
		t.Fatalf("already absent coordinator registration: %v", err)
	}
}
