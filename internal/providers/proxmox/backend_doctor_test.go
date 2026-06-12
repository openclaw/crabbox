package proxmox

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

type fakeProxmoxDoctorClient struct {
	listCalls             int
	listErr               error
	getCalls              int
	deleteCalls           int
	deletedIDs            []string
	mutated               bool
	servers               []Server
	created               Server
	createErr             error
	deleteErr             error
	deleteErrByID         map[string]error
	deleteAcceptedErrByID map[string]error
	preserveOnDeleteByID  map[string]bool
	getErrByID            map[string]error
	getErrSequenceByID    map[string][]error
	getCallsByID          map[string]int
	getServerByID         map[string]Server
	setLabels             []map[string]string
	readiness             []core.ProxmoxReadinessCheck
	leaseIDs              []string
}

func (c *fakeProxmoxDoctorClient) DoctorReadiness(context.Context, Config) ([]core.ProxmoxReadinessCheck, error) {
	return c.readiness, nil
}

func (c *fakeProxmoxDoctorClient) ListCrabboxServers(context.Context) ([]Server, error) {
	c.listCalls++
	return c.servers, c.listErr
}

func (c *fakeProxmoxDoctorClient) CreateServer(_ context.Context, _ Config, _ string, leaseID string, _ string, _ bool) (Server, error) {
	c.mutated = true
	c.leaseIDs = append(c.leaseIDs, leaseID)
	if c.createErr != nil {
		return Server{}, c.createErr
	}
	if c.created.CloudID != "" {
		return c.created, nil
	}
	return Server{}, nil
}

func (c *fakeProxmoxDoctorClient) GetServer(_ context.Context, id string) (Server, error) {
	c.getCalls++
	if c.getCallsByID == nil {
		c.getCallsByID = map[string]int{}
	}
	callIndex := c.getCallsByID[id]
	c.getCallsByID[id]++
	if sequence := c.getErrSequenceByID[id]; callIndex < len(sequence) {
		if err := sequence[callIndex]; err != nil {
			return Server{}, err
		}
	}
	if err := c.getErrByID[id]; err != nil {
		return Server{}, err
	}
	if server, ok := c.getServerByID[id]; ok {
		return server, nil
	}
	for _, server := range c.servers {
		if server.CloudID == id {
			return server, nil
		}
	}
	if c.getCalls < 3 {
		return Server{CloudID: "101", Labels: map[string]string{"lease": "cbx_test", "slug": "test"}}, nil
	}
	server := Server{CloudID: "101", Labels: map[string]string{"lease": "cbx_test", "slug": "test"}}
	server.PublicNet.IPv4.IP = "192.0.2.10"
	return server, nil
}

func (c *fakeProxmoxDoctorClient) DeleteServer(_ context.Context, id string) error {
	c.deleteCalls++
	c.deletedIDs = append(c.deletedIDs, id)
	c.mutated = true
	if c.deleteErr != nil {
		return c.deleteErr
	}
	if err := c.deleteErrByID[id]; err != nil {
		return err
	}
	acceptedErr := c.deleteAcceptedErrByID[id]
	if c.preserveOnDeleteByID[id] {
		return acceptedErr
	}
	for i, server := range c.servers {
		if server.CloudID == id {
			c.servers = append(c.servers[:i], c.servers[i+1:]...)
			break
		}
	}
	return acceptedErr
}

func (c *fakeProxmoxDoctorClient) SetLabels(_ context.Context, _ string, labels map[string]string) error {
	c.mutated = true
	c.setLabels = append(c.setLabels, map[string]string{})
	for key, value := range labels {
		c.setLabels[len(c.setLabels)-1][key] = value
	}
	return nil
}

func TestProxmoxDoctorReportsReadinessChecksWithoutMutation(t *testing.T) {
	fake := &fakeProxmoxDoctorClient{readiness: []core.ProxmoxReadinessCheck{
		{Status: "ok", Check: "auth", Message: "auth=ready endpoint=/version", Details: map[string]string{"auth": "ready", "endpoint": "/version"}},
		{Status: "ok", Check: "node", Message: "node=pve endpoint=/nodes/pve/status", Details: map[string]string{"node": "pve", "endpoint": "/nodes/pve/status"}},
		{Status: "ok", Check: "storage", Message: "storage=local-lvm active=1 enabled=1", Details: map[string]string{"storage": "local-lvm"}},
		{Status: "ok", Check: "bridge", Message: "bridge=vmbr0 type=bridge", Details: map[string]string{"bridge": "vmbr0"}},
		{Status: "ok", Check: "template", Message: "templateId=9000 template=ready", Details: map[string]string{"templateId": "9000"}},
		{Status: "ok", Check: "nextid", Message: "nextid=101 endpoint=/cluster/nextid", Details: map[string]string{"nextid": "101"}},
		{Status: "ok", Check: "inventory", Message: "api=list mutation=false leases=1 vms=2", Details: map[string]string{"api": "list", "mutation": "false", "leases": "1"}},
		{Status: "ok", Check: "mutation", Message: "mutation=false", Details: map[string]string{"mutation": "false"}},
	}}
	old := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = old })

	doctor, err := Provider{}.ConfigureDoctor(Config{}, Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := doctor.Doctor(context.Background(), core.DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Provider != "proxmox" || len(result.Checks) != len(fake.readiness) {
		t.Fatalf("result=%#v", result)
	}
	for _, want := range []string{"auth", "node", "storage", "bridge", "template", "nextid", "inventory", "mutation"} {
		found := false
		for _, check := range result.Checks {
			if check.Check == want {
				found = true
				if check.Details["provider"] != "" {
					t.Fatalf("backend should not pre-fill provider detail: %#v", check)
				}
			}
		}
		if !found {
			t.Fatalf("missing check %q in %#v", want, result.Checks)
		}
	}
	if fake.listCalls != 0 {
		t.Fatalf("list calls=%d, want 0 through backend doctor", fake.listCalls)
	}
	if fake.mutated {
		t.Fatal("doctor called a mutating Proxmox method")
	}
}

func TestProxmoxAcquireRejectsMissingTemplateBeforeClientWork(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	clientCalls := 0
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		clientCalls++
		return &fakeProxmoxDoctorClient{}, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root"}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Acquire(context.Background(), AcquireRequest{}); err == nil || !strings.Contains(err.Error(), "proxmox templateId is required") {
		t.Fatalf("Acquire error=%v, want missing templateId", err)
	}
	if clientCalls != 0 {
		t.Fatalf("newClient calls=%d, want 0 before template validation", clientCalls)
	}
}

func TestProxmoxAcquirePollsUntilServerIPIsAvailable(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeProxmoxDoctorClient{}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(_ context.Context, target *SSHTarget, _ io.Writer, _ string, _ time.Duration) error {
		if target.Host != "192.0.2.10" {
			t.Fatalf("ssh host=%q, want discovered IP", target.Host)
		}
		return nil
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })
	oldPoll := proxmoxIPPollInterval
	proxmoxIPPollInterval = time.Millisecond
	t.Cleanup(func() { proxmoxIPPollInterval = oldPoll })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root", Proxmox: core.ProxmoxConfig{TemplateID: 9400}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.PublicNet.IPv4.IP != "192.0.2.10" {
		t.Fatalf("ip=%q, want discovered IP", target.Server.PublicNet.IPv4.IP)
	}
	if fake.getCalls != 3 {
		t.Fatalf("getCalls=%d, want 3", fake.getCalls)
	}
	if fake.deleteCalls != 0 {
		t.Fatal("delayed IP discovery should not delete the VM")
	}
}

func TestProxmoxAcquireInitializesNilLabels(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	created := Server{CloudID: "101"}
	created.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeProxmoxDoctorClient{
		created: created,
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return nil
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root", Proxmox: core.ProxmoxConfig{TemplateID: 9400}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Acquire(context.Background(), AcquireRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.Labels["state"] != "ready" {
		t.Fatalf("labels=%v, want state=ready", target.Server.Labels)
	}
	if len(fake.setLabels) != 1 || fake.setLabels[0]["state"] != "ready" {
		t.Fatalf("setLabels=%v, want state=ready", fake.setLabels)
	}
}

func TestProxmoxAcquireSSHFailureRemovesStoredKeyAfterDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	created := Server{CloudID: "101"}
	created.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeProxmoxDoctorClient{created: created}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root", Proxmox: core.ProxmoxConfig{TemplateID: 9400}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Acquire(context.Background(), AcquireRequest{}); err == nil {
		t.Fatal("expected ssh readiness failure")
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if len(fake.leaseIDs) != 1 {
		t.Fatalf("leaseIDs=%v, want one generated lease", fake.leaseIDs)
	}
	assertStoredTestboxKeyRemoved(t, fake.leaseIDs[0])
}

func TestProxmoxAcquirePreservesStoredKeyWhenDeleteFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	created := Server{CloudID: "101"}
	created.PublicNet.IPv4.IP = "192.0.2.10"
	fake := &fakeProxmoxDoctorClient{created: created, deleteErr: errors.New("delete failed")}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root", Proxmox: core.ProxmoxConfig{TemplateID: 9400}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Acquire(context.Background(), AcquireRequest{}); err == nil {
		t.Fatal("expected ssh readiness failure")
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if len(fake.leaseIDs) != 1 {
		t.Fatalf("leaseIDs=%v, want one generated lease", fake.leaseIDs)
	}
	assertStoredTestboxKeyExists(t, fake.leaseIDs[0])
}

func TestProxmoxCleanupRemovesClaimAfterDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_cleanup"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{server}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 1 {
		t.Fatalf("deleteCalls=%d, want 1", fake.deleteCalls)
	}
	if fake.listCalls != 1 {
		t.Fatalf("listCalls=%d, want one pre-delete inventory", fake.listCalls)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupRemovesUniqueLegacyClaimWithoutCloudID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_legacy"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupRemovesStoredKeyWhenClaimIsMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_missing_claim"
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupDryRunPreservesClaim(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_dryrun"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if fake.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d, want 0", fake.deleteCalls)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v, want preserved", ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupPreservesResidueForMismatchedClaimCloudID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_mismatch"
	claimServer := expiredProxmoxServer("202", leaseID)
	claimServer.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, claimServer, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", leaseID)}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v, want preserved", ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupRemovesMismatchedClaimWhenClaimedVMIsAlsoMissing(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_mismatch_missing"
	claimServer := expiredProxmoxServer("202", leaseID)
	claimServer.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, claimServer, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		servers: []Server{expiredProxmoxServer("101", leaseID)},
		getErrByID: map[string]error{
			"202": &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/202/status/current", StatusCode: 404, Body: "not found"},
		},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after both cloud IDs are missing", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupPreservesResidueForDuplicateRemoteLeaseLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_duplicate"
	expired := expiredProxmoxServer("101", leaseID)
	active := expiredProxmoxServer("202", leaseID)
	delete(active.Labels, "expires_at")
	active.Labels["keep"] = "true"
	active.Provider = "proxmox"
	active.PublicNet.IPv4.IP = "192.0.2.202"
	expired.PublicNet.IPv4.IP = "192.0.2.101"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, active, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expired, active}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v, want preserved", ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupRetargetsClaimToSoleSurvivingDuplicate(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_retarget"
	expired := expiredProxmoxServer("101", leaseID)
	expired.Provider = "proxmox"
	active := expiredProxmoxServer("202", leaseID)
	delete(active.Labels, "expires_at")
	active.Labels["keep"] = "true"
	active.Provider = "proxmox"
	active.PublicNet.IPv4.IP = "192.0.2.202"
	expired.PublicNet.IPv4.IP = "192.0.2.101"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, expired, SSHTarget{Host: expired.PublicNet.IPv4.IP, Port: "22"}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{expired, active}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHPort: "2222"}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "202" || claim.SSHHost != "192.0.2.202" || claim.SSHPort != 22 {
		t.Fatalf("claim=%#v ok=%t err=%v, want surviving cloud id with known working SSH port", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupRemovesClaimWhenSoleSurvivorAlsoDisappears(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_survivor_gone"
	expired := expiredProxmoxServer("101", leaseID)
	active := expiredProxmoxServer("202", leaseID)
	delete(active.Labels, "expires_at")
	active.Labels["keep"] = "true"
	active.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, active, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		servers: []Server{expired, active},
		getErrByID: map[string]error{
			"202": &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/202/status/current", StatusCode: 404, Body: "not found"},
		},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after both VMs disappeared", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupPreservesClaimWhenSurvivorOwnershipCannotBeVerified(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_survivor_unverified"
	expired := expiredProxmoxServer("101", leaseID)
	expired.Provider = "proxmox"
	active := expiredProxmoxServer("202", leaseID)
	delete(active.Labels, "expires_at")
	active.Labels["keep"] = "true"
	active.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, expired, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	verified := active
	verified.Labels = map[string]string{"node": "pve1"}
	fake := &fakeProxmoxDoctorClient{
		servers:       []Server{expired, active},
		getServerByID: map[string]Server{"202": verified},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "101" {
		t.Fatalf("claim=%#v ok=%t err=%v, want original claim preserved", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupRetargetsStaleMissingClaimToVerifiedSurvivor(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_stale_retarget"
	expired := expiredProxmoxServer("101", leaseID)
	active := expiredProxmoxServer("202", leaseID)
	delete(active.Labels, "expires_at")
	active.Labels["keep"] = "true"
	active.Provider = "proxmox"
	active.PublicNet.IPv4.IP = "192.0.2.202"
	stale := expiredProxmoxServer("303", leaseID)
	stale.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, stale, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		servers: []Server{expired, active},
		getErrByID: map[string]error{
			"303": &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/303/status/current", StatusCode: 404, Body: "not found"},
		},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "202" || claim.SSHHost != "192.0.2.202" {
		t.Fatalf("claim=%#v ok=%t err=%v, want verified survivor", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupRemovesResidueWhenAllDuplicateLabelsAreDeleted(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_duplicate_expired"
	first := expiredProxmoxServer("101", leaseID)
	second := expiredProxmoxServer("202", leaseID)
	second.Provider = "proxmox"
	first.PublicNet.IPv4.IP = "192.0.2.101"
	second.PublicNet.IPv4.IP = "192.0.2.202"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, second, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{first, second}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 2 {
		t.Fatalf("deletedIDs=%v, want both duplicates deleted", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupReconcilesSuccessfulDeletesBeforeLaterFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	firstLeaseID := "cbx_proxmox_partial_first"
	secondLeaseID := "cbx_proxmox_partial_second"
	first := expiredProxmoxServer("101", firstLeaseID)
	first.Provider = "proxmox"
	second := expiredProxmoxServer("202", secondLeaseID)
	second.Provider = "proxmox"
	for _, item := range []struct {
		leaseID string
		server  Server
	}{
		{firstLeaseID, first},
		{secondLeaseID, second},
	} {
		if err := core.ClaimLeaseTargetForRepoConfig(item.leaseID, "old", Config{Provider: "proxmox"}, item.server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
		if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, item.leaseID); err != nil {
			t.Fatal(err)
		}
	}
	fake := &fakeProxmoxDoctorClient{
		servers:       []Server{first, second},
		deleteErrByID: map[string]error{"202": errors.New("delete failed")},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err == nil {
		t.Fatal("expected later delete failure")
	}
	if _, ok, err := core.ResolveLeaseClaim(firstLeaseID); err != nil || ok {
		t.Fatalf("first claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, firstLeaseID)
	if _, ok, err := core.ResolveLeaseClaim(secondLeaseID); err != nil || !ok {
		t.Fatalf("second claim ok=%t err=%v, want preserved", ok, err)
	}
	assertStoredTestboxKeyExists(t, secondLeaseID)
}

func TestProxmoxCleanupRetargetsDuplicateClaimBeforeReturningDeleteFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_partial_duplicate"
	first := expiredProxmoxServer("101", leaseID)
	first.Provider = "proxmox"
	second := expiredProxmoxServer("202", leaseID)
	second.Provider = "proxmox"
	first.PublicNet.IPv4.IP = "192.0.2.101"
	second.PublicNet.IPv4.IP = "192.0.2.202"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, first, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		servers:       []Server{first, second},
		deleteErrByID: map[string]error{"202": errors.New("delete failed")},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err == nil {
		t.Fatal("expected second delete failure")
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "202" || claim.SSHHost != "192.0.2.202" {
		t.Fatalf("claim=%#v ok=%t err=%v, want retargeted surviving duplicate", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupReconcilesDeleteAcceptedBeforePollingFailure(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_accepted_delete"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		servers:               []Server{server},
		deleteAcceptedErrByID: map[string]error{"101": &core.ProxmoxDeleteTaskError{Err: errors.New("task status timeout")}},
		getErrByID: map[string]error{
			"101": &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"},
		},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	err := backend.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "task status timeout") {
		t.Fatalf("cleanup error=%v, want polling failure", err)
	}
	if fake.listCalls != 1 || fake.getCalls != 1 {
		t.Fatalf("listCalls=%d getCalls=%d, want one inventory and one authoritative verification", fake.listCalls, fake.getCalls)
	}
	if _, ok, resolveErr := core.ResolveLeaseClaim(leaseID); resolveErr != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after confirmed disappearance", ok, resolveErr)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupReconcilesDeleteThatCompletesAfterInitialVerification(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldPollInterval := proxmoxDeleteVerifyPollInterval
	oldTimeout := proxmoxDeleteVerifyTimeout
	proxmoxDeleteVerifyPollInterval = time.Millisecond
	proxmoxDeleteVerifyTimeout = time.Second
	t.Cleanup(func() {
		proxmoxDeleteVerifyPollInterval = oldPollInterval
		proxmoxDeleteVerifyTimeout = oldTimeout
	})
	leaseID := "cbx_proxmox_eventual_delete"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	notFound := &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"}
	fake := &fakeProxmoxDoctorClient{
		servers:               []Server{server},
		deleteAcceptedErrByID: map[string]error{"101": &core.ProxmoxDeleteTaskError{Err: errors.New("task status timeout")}},
		preserveOnDeleteByID:  map[string]bool{"101": true},
		getErrSequenceByID:    map[string][]error{"101": {nil, notFound}},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	err := backend.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "task status timeout") {
		t.Fatalf("cleanup error=%v, want polling failure", err)
	}
	if fake.getCallsByID["101"] != 2 {
		t.Fatalf("getCalls=%d, want initial existence check followed by not-found", fake.getCallsByID["101"])
	}
	if _, ok, resolveErr := core.ResolveLeaseClaim(leaseID); resolveErr != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after eventual disappearance", ok, resolveErr)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxCleanupPollsAmbiguousDeleteRequestUntilVMDisappears(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	oldPollInterval := proxmoxDeleteVerifyPollInterval
	oldTimeout := proxmoxDeleteVerifyTimeout
	proxmoxDeleteVerifyPollInterval = time.Millisecond
	proxmoxDeleteVerifyTimeout = time.Second
	t.Cleanup(func() {
		proxmoxDeleteVerifyPollInterval = oldPollInterval
		proxmoxDeleteVerifyTimeout = oldTimeout
	})
	leaseID := "cbx_proxmox_ambiguous_request"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	notFound := &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"}
	fake := &fakeProxmoxDoctorClient{
		servers:               []Server{server},
		deleteAcceptedErrByID: map[string]error{"101": &core.ProxmoxDeleteRequestError{Err: errors.New("delete request timeout")}},
		preserveOnDeleteByID:  map[string]bool{"101": true},
		getErrSequenceByID:    map[string][]error{"101": {nil, notFound}},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	err := backend.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "delete request timeout") {
		t.Fatalf("cleanup error=%v, want ambiguous request failure", err)
	}
	if fake.getCallsByID["101"] != 2 {
		t.Fatalf("getCalls=%d, want polling until not-found", fake.getCallsByID["101"])
	}
	if _, ok, resolveErr := core.ResolveLeaseClaim(leaseID); resolveErr != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after eventual disappearance", ok, resolveErr)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxReleaseRemovesClaimAndStoredKeyAfterDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_release"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: leaseID,
		Server:  expiredProxmoxServer("101", leaseID),
	}}
	if err := backend.ReleaseLease(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxReleasePreservesLocalResidueWhenDeleteFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_release_fail"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{deleteErr: errors.New("delete failed")}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{
		LeaseID: leaseID,
		Server:  expiredProxmoxServer("101", leaseID),
	}}
	if err := backend.ReleaseLease(context.Background(), req); err == nil {
		t.Fatal("expected delete failure")
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v, want preserved", ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxReleaseRetargetsClaimAndPreservesKeyForDuplicateLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_release_duplicate"
	first := expiredProxmoxServer("101", leaseID)
	first.Provider = "proxmox"
	first.PublicNet.IPv4.IP = "192.0.2.101"
	survivor := expiredProxmoxServer("202", leaseID)
	survivor.Provider = "proxmox"
	survivor.PublicNet.IPv4.IP = "192.0.2.202"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, first, SSHTarget{Host: first.PublicNet.IPv4.IP, Port: "22"}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{first, survivor}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: first}}
	if err := backend.ReleaseLease(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "202" || claim.SSHHost != "192.0.2.202" {
		t.Fatalf("claim=%#v ok=%t err=%v, want surviving duplicate", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxReleaseRetriesReconciliationAfterInventoryRefreshFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_release_inventory_failure"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{server}, listErr: errors.New("inventory unavailable")}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	var stderr strings.Builder
	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: &stderr}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}
	if err := backend.ReleaseLease(context.Background(), req); err == nil {
		t.Fatal("expected inventory reconciliation failure")
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "101" {
		t.Fatalf("claim=%#v ok=%t err=%v, want preserved until duplicate reconciliation", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
	if !strings.Contains(stderr.String(), "reason=inventory_refresh_failed") {
		t.Fatalf("stderr=%q, want reconciliation warning", stderr.String())
	}

	fake.listErr = nil
	fake.deleteErrByID = map[string]error{
		"101": &core.ProxmoxError{Method: "DELETE", Path: "/nodes/pve1/qemu/101", StatusCode: 404, Body: "not found"},
	}
	fake.getErrByID = map[string]error{
		"101": &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"},
	}
	resolved, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatalf("retry resolve: %v", err)
	}
	if resolved.LeaseID != leaseID || resolved.Server.CloudID != "101" {
		t.Fatalf("retry target=%#v", resolved)
	}
	deleteCalls := fake.deleteCalls
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: resolved}); err != nil {
		t.Fatalf("retry release: %v", err)
	}
	if fake.deleteCalls != deleteCalls {
		t.Fatalf("deleteCalls=%d, want absent target reconciliation without another delete", fake.deleteCalls)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed after successful retry reconciliation", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxReleaseOnlyClaimRecoveryRejectsReusedVMID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_reused_vmid"
	claimed := expiredProxmoxServer("101", leaseID)
	claimed.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, claimed, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	reused := Server{CloudID: "101", Provider: "proxmox", ID: 101, Name: "unrelated-vm", Labels: map[string]string{"crabbox": "false"}}
	fake := &fakeProxmoxDoctorClient{getServerByID: map[string]Server{"101": reused}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "stale local claim") {
		t.Fatalf("resolve error=%v, want stale claim rejection", err)
	}
	if fake.deleteCalls != 0 {
		t.Fatalf("deleteCalls=%d, want no deletion", fake.deleteCalls)
	}
}

func TestProxmoxReleasePreservesDifferentClaimWhenInventoryRefreshFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_release_inventory_mismatch"
	deleted := expiredProxmoxServer("101", leaseID)
	deleted.Provider = "proxmox"
	claimed := expiredProxmoxServer("202", leaseID)
	claimed.Provider = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, claimed, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{deleted, claimed}, listErr: errors.New("inventory unavailable")}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: deleted}}); err == nil {
		t.Fatal("expected inventory reconciliation failure")
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "202" {
		t.Fatalf("claim=%#v ok=%t err=%v, want different claim preserved", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxCleanupIgnoresInvalidClaimLabel(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fake := &fakeProxmoxDoctorClient{servers: []Server{expiredProxmoxServer("101", "../target")}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
}

func TestProxmoxCleanupWithoutLeaseLabelPreservesNumericClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	if err := core.ClaimLeaseForRepoProvider("101", "numeric", "proxmox", t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	server := expiredProxmoxServer("101", "")
	delete(server.Labels, "lease")
	fake := &fakeProxmoxDoctorClient{servers: []Server{server}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) {
		return fake, nil
	}
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedIDs) != 1 || fake.deletedIDs[0] != "101" {
		t.Fatalf("deletedIDs=%v, want [101]", fake.deletedIDs)
	}
	if _, ok, err := core.ResolveLeaseClaim("101"); err != nil || !ok {
		t.Fatalf("numeric claim ok=%t err=%v, want preserved", ok, err)
	}
}

func expiredProxmoxServer(id, leaseID string) Server {
	return Server{
		CloudID: id,
		Name:    "crabbox-old",
		Labels: map[string]string{
			"lease":      leaseID,
			"slug":       "old",
			"keep":       "false",
			"state":      "ready",
			"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		},
	}
}

func assertStoredTestboxKeyExists(t *testing.T, leaseID string) {
	t.Helper()
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("stored key %s stat error: %v", keyPath, err)
	}
}

func assertStoredTestboxKeyRemoved(t *testing.T, leaseID string) {
	t.Helper()
	keyPath, err := core.TestboxKeyPath(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stored key %s stat err=%v, want not exist", keyPath, err)
	}
}
