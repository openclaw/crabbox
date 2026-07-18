package incus

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lxc/incus/v7/shared/api"
	core "github.com/openclaw/crabbox/internal/cli"
)

type afterListClient struct {
	*fakeClient
	afterList func()
}

func (c *afterListClient) ListInstances() ([]api.Instance, error) {
	instances, err := c.fakeClient.ListInstances()
	if err == nil && c.afterList != nil {
		c.afterList()
	}
	return instances, err
}

func TestCleanupPreservesInstanceReclaimedDuringList(t *testing.T) {
	testCleanupPreservesInstanceReclaimedDuringList(t, false, true)
}

func TestCleanupDryRunDoesNotPlanReclaimedInstanceRemoval(t *testing.T) {
	testCleanupPreservesInstanceReclaimedDuringList(t, true, true)
}

func TestCleanupPreservesReclaimedInstanceBeforeClaimPublish(t *testing.T) {
	testCleanupPreservesInstanceReclaimedDuringList(t, false, false)
}

func TestIncusLeaseOperationLockSerializesReclaimAndCleanup(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))

	const (
		leaseID = "cbx_incus_lock"
		name    = "crabbox-incus-lock"
	)
	unlockReclaim, err := lockIncusLeaseOperation(context.Background(), leaseID, name)
	if err != nil {
		t.Fatalf("lock reclaim operation: %v", err)
	}
	defer unlockReclaim()

	cleanupAcquired := make(chan struct{})
	cleanupDone := make(chan struct{})
	go func() {
		defer close(cleanupDone)
		unlockCleanup, lockErr := lockIncusLeaseOperation(context.Background(), leaseID, name)
		if lockErr != nil {
			t.Errorf("lock cleanup operation: %v", lockErr)
			return
		}
		close(cleanupAcquired)
		unlockCleanup()
	}()

	select {
	case <-cleanupAcquired:
		t.Fatal("cleanup operation acquired lock while reclaim still held it")
	case <-time.After(100 * time.Millisecond):
	}
	unlockReclaim()
	select {
	case <-cleanupDone:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup operation did not acquire lock after reclaim released it")
	}
}

func testCleanupPreservesInstanceReclaimedDuringList(t *testing.T, dryRun, publishClaim bool) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))

	const (
		leaseID = "cbx_incus_reclaimed"
		slug    = "incus-reclaimed"
		name    = "crabbox-incus-reclaimed"
	)
	now := time.Now().UTC()
	staleLabels := map[string]string{
		"crabbox":           "true",
		"provider":          providerName,
		"lease":             leaseID,
		"slug":              slug,
		"state":             "expired",
		"created_at":        core.LeaseLabelTime(now.Add(-2 * time.Hour)),
		"last_touched_at":   core.LeaseLabelTime(now.Add(-2 * time.Hour)),
		"idle_timeout_secs": "60",
		"ttl_secs":          "120",
		"expires_at":        core.LeaseLabelTime(now.Add(-time.Hour)),
	}
	instanceConfig := map[string]string{}
	for key, value := range staleLabels {
		instanceConfig[labelKey(key)] = value
	}
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			name: {
				Name:        name,
				Status:      "Stopped",
				StatusCode:  api.Stopped,
				InstancePut: api.InstancePut{Config: instanceConfig},
			},
		},
		states: map[string]*api.InstanceState{
			name: {Status: "Stopped", StatusCode: api.Stopped},
		},
	}

	repoRoot := t.TempDir()
	staleServer := core.Server{
		CloudID:  name,
		Provider: providerName,
		Name:     name,
		Status:   "stopped",
		Labels:   staleLabels,
	}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, slug, providerName, instanceScope(name), "", repoRoot,
		time.Minute, false, staleServer, core.SSHTarget{},
	); err != nil {
		t.Fatalf("write initial claim: %v", err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir key dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("prepared-by-concurrent-acquire"), 0o600); err != nil {
		t.Fatalf("write stored key: %v", err)
	}

	client := &afterListClient{fakeClient: fake}
	client.afterList = func() {
		freshLabels := core.TouchDirectLeaseLabels(staleLabels, core.BaseConfig(), "ready", time.Now().UTC())
		freshServer := core.Server{
			CloudID:  name,
			Provider: providerName,
			Name:     name,
			Status:   "ready",
			Labels:   freshLabels,
		}
		if publishClaim {
			if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
				leaseID, slug, providerName, instanceScope(name), "", repoRoot,
				5*time.Minute, true, freshServer, core.SSHTarget{},
			); err != nil {
				t.Fatalf("reclaim during ListInstances: %v", err)
			}
		}
		freshConfig := cloneMap(instanceConfig)
		for key, value := range freshLabels {
			freshConfig[labelKey(key)] = value
		}
		fake.instances[name].Config = freshConfig
		fake.instances[name].Status = "Running"
		fake.instances[name].StatusCode = api.Running
		fake.states[name] = &api.InstanceState{Status: "Running", StatusCode: api.Running}
	}

	oldNewClient := newClient
	newClient = func(Config) (instanceClient, error) { return client, nil }
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	var out bytes.Buffer
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &out, Stderr: &out}).(*backend)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{DryRun: dryRun}); err != nil {
		t.Fatalf("Cleanup: %v\n%s", err, out.String())
	}

	if len(fake.deleted) != 0 {
		t.Fatalf("Cleanup deleted concurrently reclaimed instance: %v\n%s", fake.deleted, out.String())
	}
	claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil {
		t.Fatalf("resolve reclaimed claim: %v", err)
	}
	if !ok || claim.LeaseID != leaseID {
		t.Fatalf("Cleanup removed concurrently reclaimed claim: present=%v claim=%#v\n%s", ok, claim, out.String())
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("Cleanup removed key prepared by concurrent Acquire: %v", err)
	}
	if !strings.Contains(out.String(), "changed-during-cleanup") {
		t.Fatalf("Cleanup did not report the reclaimed instance guard: %s", out.String())
	}
	if strings.Contains(out.String(), "would remove instance name="+name) {
		t.Fatalf("Cleanup planned removal of a reclaimed instance: %s", out.String())
	}
}

func TestCleanupRemovesUnchangedClaimedExpiredInstance(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))

	const (
		leaseID = "cbx_incus_orphan"
		slug    = "incus-orphan"
		name    = "crabbox-incus-orphan"
	)
	now := time.Now().UTC()
	labels := map[string]string{
		"crabbox":           "true",
		"provider":          providerName,
		"lease":             leaseID,
		"slug":              slug,
		"state":             "expired",
		"created_at":        core.LeaseLabelTime(now.Add(-2 * time.Hour)),
		"last_touched_at":   core.LeaseLabelTime(now.Add(-2 * time.Hour)),
		"idle_timeout_secs": "60",
		"ttl_secs":          "120",
		"expires_at":        core.LeaseLabelTime(now.Add(-time.Hour)),
	}
	config := map[string]string{}
	for key, value := range labels {
		config[labelKey(key)] = value
	}
	fake := &fakeClient{
		instances: map[string]*api.Instance{
			name: {
				Name:        name,
				Status:      "Stopped",
				StatusCode:  api.Stopped,
				InstancePut: api.InstancePut{Config: config},
			},
		},
		states: map[string]*api.InstanceState{
			name: {Status: "Stopped", StatusCode: api.Stopped},
		},
	}
	repoRoot := t.TempDir()
	server := core.Server{CloudID: name, Provider: providerName, Name: name, Status: "stopped", Labels: labels}
	if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
		leaseID, slug, providerName, instanceScope(name), "", repoRoot,
		time.Minute, false, server, core.SSHTarget{},
	); err != nil {
		t.Fatalf("write orphan claim: %v", err)
	}
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatalf("TestboxKeyPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatalf("mkdir key dir: %v", err)
	}
	if err := os.WriteFile(keyPath, []byte("stored-key"), 0o600); err != nil {
		t.Fatalf("write stored key: %v", err)
	}

	oldNewClient := newClient
	newClient = func(Config) (instanceClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldNewClient })

	cfg := core.BaseConfig()
	cfg.Provider = providerName
	b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).(*backend)
	if err := b.Cleanup(context.Background(), core.CleanupRequest{}); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if len(fake.deleted) != 1 || fake.deleted[0] != name {
		t.Fatalf("deleted=%v want [%s]", fake.deleted, name)
	}
	if claim, ok, err := core.ResolveLeaseClaimForProvider(leaseID, providerName); err != nil {
		t.Fatalf("resolve removed claim: %v", err)
	} else if ok || claim.LeaseID != "" {
		t.Fatalf("claim still present after legitimate cleanup: %#v", claim)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key should be retained after cleanup: %v", err)
	}
}
