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
	clusterListErr        error
	getCalls              int
	deleteCalls           int
	deletedIDs            []string
	deletedNodes          []string
	mutated               bool
	servers               []Server
	clusterServers        []Server
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
	clusterExistsByID     map[string]bool
	clusterExistsErr      error
	setLabels             []map[string]string
	labelNodes            []string
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

func (c *fakeProxmoxDoctorClient) ListCrabboxServersCluster(context.Context) ([]Server, error) {
	c.listCalls++
	if c.clusterListErr != nil && (c.listCalls > 1 || len(c.servers) == 0) {
		return nil, c.clusterListErr
	}
	if c.clusterServers != nil && (c.listCalls > 1 || len(c.servers) == 0) {
		return c.clusterServers, nil
	}
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

func (c *fakeProxmoxDoctorClient) GetServerOnNode(ctx context.Context, node, id string) (Server, error) {
	for _, server := range c.clusterServers {
		if server.CloudID == id && (server.HostID == "" || server.HostID == node) {
			return server, nil
		}
	}
	return c.GetServer(ctx, id)
}

func (c *fakeProxmoxDoctorClient) VMExistsInCluster(_ context.Context, id string) (bool, error) {
	if c.clusterExistsErr != nil {
		return false, c.clusterExistsErr
	}
	if exists, ok := c.clusterExistsByID[id]; ok {
		return exists, nil
	}
	if err := c.getErrByID[id]; err != nil {
		if core.IsProxmoxNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if _, ok := c.getServerByID[id]; ok {
		return true, nil
	}
	for _, server := range append(append([]Server(nil), c.servers...), c.clusterServers...) {
		if server.CloudID == id {
			return true, nil
		}
	}
	return false, nil
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

func (c *fakeProxmoxDoctorClient) DeleteServerOnNode(ctx context.Context, node, id string) error {
	c.deletedNodes = append(c.deletedNodes, node)
	if err := c.DeleteServer(ctx, id); err != nil {
		return err
	}
	for i, server := range c.clusterServers {
		if server.CloudID == id && (server.HostID == "" || server.HostID == node) {
			c.clusterServers = append(c.clusterServers[:i], c.clusterServers[i+1:]...)
			break
		}
	}
	return nil
}

func (c *fakeProxmoxDoctorClient) DeleteServerOnNodeChecked(ctx context.Context, node, id string, check func(Server) error) error {
	server, err := c.GetServerOnNode(ctx, node, id)
	if err != nil {
		return err
	}
	if err := check(server); err != nil {
		return err
	}
	return c.DeleteServerOnNode(ctx, node, id)
}

func (c *fakeProxmoxDoctorClient) SetLabels(_ context.Context, _ string, labels map[string]string) error {
	c.mutated = true
	c.setLabels = append(c.setLabels, map[string]string{})
	for key, value := range labels {
		c.setLabels[len(c.setLabels)-1][key] = value
	}
	return nil
}

func (c *fakeProxmoxDoctorClient) SetLabelsOnNode(ctx context.Context, node, id string, labels map[string]string) error {
	c.labelNodes = append(c.labelNodes, node)
	return c.SetLabels(ctx, id, labels)
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

func TestProxmoxTouchUsesMigratedVMNode(t *testing.T) {
	fake := &fakeProxmoxDoctorClient{}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	server := expiredProxmoxServer("101", "cbx_migrated_touch")
	server.HostID = "pve2"
	backend := NewLeaseBackend(Provider{}.Spec(), Config{Proxmox: core.ProxmoxConfig{Node: "pve1"}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Touch(context.Background(), TouchRequest{Lease: LeaseTarget{LeaseID: "cbx_migrated_touch", Server: server}, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.labelNodes) != 1 || fake.labelNodes[0] != "pve2" {
		t.Fatalf("labelNodes=%v, want [pve2]", fake.labelNodes)
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

func TestProxmoxAcquirePreservesStoredKeyWhenVMClaimsToMigrateDuringCleanup(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	created := expiredProxmoxServer("101", "cbx_pending")
	created.HostID = "pve1"
	created.PublicNet.IPv4.IP = "192.0.2.10"
	migrated := created
	migrated.HostID = "pve2"
	fake := &fakeProxmoxDoctorClient{created: created, clusterServers: []Server{migrated}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })
	oldWait := waitForSSHReadyFunc
	waitForSSHReadyFunc = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		return errors.New("ssh unavailable")
	}
	t.Cleanup(func() { waitForSSHReadyFunc = oldWait })

	backend := NewLeaseBackend(Provider{}.Spec(), Config{SSHUser: "root", Proxmox: core.ProxmoxConfig{Node: "pve1", TemplateID: 9400}}, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Acquire(context.Background(), AcquireRequest{}); err == nil {
		t.Fatal("expected ssh readiness failure")
	}
	if len(fake.deletedNodes) != 1 || fake.deletedNodes[0] != "pve1" {
		t.Fatalf("deletedNodes=%v, want [pve1]", fake.deletedNodes)
	}
	if len(fake.leaseIDs) != 1 {
		t.Fatalf("leaseIDs=%v, want one generated lease", fake.leaseIDs)
	}
	assertStoredTestboxKeyExists(t, fake.leaseIDs[0])
}

func TestProxmoxReleaseRemovesClaimAndStoredKeyAfterDelete(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
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

	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
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
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
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

	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
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

func TestProxmoxReleaseCannotRetargetClaimToDuplicateLabel(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
	leaseID := "cbx_proxmox_release_duplicate"
	first := expiredProxmoxServer("101", leaseID)
	first.Provider = "proxmox"
	first.PublicNet.IPv4.IP = "192.0.2.101"
	survivor := expiredProxmoxServer("202", leaseID)
	survivor.Provider = "proxmox"
	survivor.HostID = "pve2"
	survivor.PublicNet.IPv4.IP = "192.0.2.202"
	first.HostID = "pve1"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", Config{Provider: "proxmox"}, first, SSHTarget{Host: first.PublicNet.IPv4.IP, Port: "22"}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{first}, clusterServers: []Server{survivor}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: first}}
	if err := backend.ReleaseLease(context.Background(), req); err == nil || !strings.Contains(err.Error(), "surviving VM") {
		t.Fatalf("release error=%v, want surviving VM failure", err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "101" || claim.SSHHost != "192.0.2.101" {
		t.Fatalf("claim VMID=%q host=%q ok=%t err=%v, want original binding preserved", claim.CloudID, claim.SSHHost, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxReleasePreservesMigratedTargetWithSameVMID(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
	leaseID := "cbx_proxmox_release_migrated"
	first := expiredProxmoxServer("101", leaseID)
	first.Provider = "proxmox"
	first.HostID = "pve1"
	first.PublicNet.IPv4.IP = "192.0.2.101"
	migrated := first
	migrated.HostID = "pve2"
	migrated.PublicNet.IPv4.IP = "192.0.2.202"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", cfg, first, SSHTarget{Host: first.PublicNet.IPv4.IP, Port: "22"}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{servers: []Server{first}, clusterServers: []Server{migrated}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: first}}); err == nil || !strings.Contains(err.Error(), "surviving VM") {
		t.Fatalf("release error=%v, want migrated survivor failure", err)
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "101" || claim.SSHHost != "192.0.2.202" {
		t.Fatalf("claim=%#v ok=%t err=%v, want migrated target", claim, ok, err)
	}
	assertStoredTestboxKeyExists(t, leaseID)
}

func TestProxmoxReleaseResolvesAndDeletesMigratedTarget(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
	leaseID := "cbx_proxmox_resolve_migrated"
	claimed := expiredProxmoxServer("101", leaseID)
	claimed.Provider = "proxmox"
	claimed.HostID = "pve1"
	claimed.PublicNet.IPv4.IP = "192.0.2.101"
	migrated := claimed
	migrated.HostID = "pve2"
	migrated.PublicNet.IPv4.IP = "192.0.2.202"
	migrated.Labels["crabbox"] = "true"
	migrated.Labels["provider"] = "proxmox"
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", cfg, claimed, SSHTarget{Host: claimed.PublicNet.IPv4.IP, Port: "22"}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := core.EnsureTestboxKeyForConfig(Config{}, leaseID); err != nil {
		t.Fatal(err)
	}
	fake := &fakeProxmoxDoctorClient{
		clusterServers: []Server{migrated},
		getErrByID:     map[string]error{"101": errors.New("source node unavailable")},
	}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.Server.CloudID != "101" || target.Server.HostID != "pve2" {
		t.Fatalf("target=%#v, want migrated VM on pve2", target)
	}
	if err := backend.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: target}); err != nil {
		t.Fatal(err)
	}
	if len(fake.deletedNodes) != 1 || fake.deletedNodes[0] != "pve2" {
		t.Fatalf("deletedNodes=%v, want [pve2]", fake.deletedNodes)
	}
	if _, ok, err := core.ResolveLeaseClaim(leaseID); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v, want removed", ok, err)
	}
	assertStoredTestboxKeyRemoved(t, leaseID)
}

func TestProxmoxReleaseRetriesReconciliationAfterInventoryRefreshFails(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve.example.test:8006", Node: "pve1"}}
	leaseID := "cbx_proxmox_release_inventory_failure"
	server := expiredProxmoxServer("101", leaseID)
	server.Provider = "proxmox"
	if err := core.ClaimLeaseForRepoProvider(leaseID, "old", "proxmox", t.TempDir(), time.Minute, false); err != nil {
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
	backend := NewLeaseBackend(Provider{}.Spec(), cfg, Runtime{Stdout: io.Discard, Stderr: &stderr}).(*leaseBackend)
	req := ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}
	if err := backend.ReleaseLease(context.Background(), req); err == nil {
		t.Fatal("expected inventory reconciliation failure")
	}
	claim, ok, err := core.ResolveLeaseClaim(leaseID)
	if err != nil || !ok || claim.CloudID != "101" {
		t.Fatalf("claim=%#v ok=%t err=%v, want preserved until duplicate reconciliation", claim, ok, err)
	}
	if claim.ProviderScope != core.ProviderClaimScope("proxmox", cfg) || claim.CloudID != "101" {
		t.Fatalf("claim scope=%q cloudID=%q, want legacy claim backfilled before deletion", claim.ProviderScope, claim.CloudID)
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

func TestProxmoxReleaseOnlyClaimRecoveryRequiresOriginalScopeAndClusterAbsence(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	leaseID := "cbx_proxmox_scoped_absence"
	claimed := expiredProxmoxServer("101", leaseID)
	claimed.Provider = "proxmox"
	original := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve-a.example.test:8006", Node: "pve1"}}
	if err := core.ClaimLeaseTargetForRepoConfig(leaseID, "old", original, claimed, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
		t.Fatal(err)
	}
	notFound := &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"}
	fake := &fakeProxmoxDoctorClient{getErrByID: map[string]error{"101": notFound}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	for name, changed := range map[string]Config{
		"cluster": func() Config {
			cfg := original
			cfg.Proxmox.APIURL = "https://pve-b.example.test:8006"
			return cfg
		}(),
		"node": func() Config {
			cfg := original
			cfg.Proxmox.Node = "pve2"
			return cfg
		}(),
	} {
		backend := NewLeaseBackend(Provider{}.Spec(), changed, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
		if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "unverified cluster scope") {
			t.Fatalf("%s-changed resolve error=%v", name, err)
		}
	}

	fake.clusterExistsByID = map[string]bool{"101": true}
	backend := NewLeaseBackend(Provider{}.Spec(), original, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	if _, err := backend.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReleaseOnly: true}); err == nil || !strings.Contains(err.Error(), "still exists in the cluster") {
		t.Fatalf("migrated-vm resolve error=%v", err)
	}
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

func TestProxmoxReleaseOnlyNumericClaimUsesCurrentClusterScope(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfgA := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve-a.example.test:8006", Node: "pve1"}}
	cfgB := Config{Provider: "proxmox", Proxmox: core.ProxmoxConfig{APIURL: "https://pve-b.example.test:8006", Node: "pve1"}}
	for _, item := range []struct {
		leaseID string
		cfg     Config
	}{
		{leaseID: "cbx_cluster_a", cfg: cfgA},
		{leaseID: "cbx_cluster_b", cfg: cfgB},
	} {
		server := expiredProxmoxServer("101", item.leaseID)
		server.Provider = "proxmox"
		if err := core.ClaimLeaseTargetForRepoConfig(item.leaseID, "old", item.cfg, server, SSHTarget{}, t.TempDir(), time.Minute, false); err != nil {
			t.Fatal(err)
		}
	}
	notFound := &core.ProxmoxError{Method: "GET", Path: "/nodes/pve1/qemu/101/status/current", StatusCode: 404, Body: "not found"}
	fake := &fakeProxmoxDoctorClient{getErrByID: map[string]error{"101": notFound}}
	oldClient := newClient
	newClient = func(Config) (proxmoxClient, error) { return fake, nil }
	t.Cleanup(func() { newClient = oldClient })

	backend := NewLeaseBackend(Provider{}.Spec(), cfgA, Runtime{Stdout: io.Discard, Stderr: io.Discard}).(*leaseBackend)
	target, err := backend.Resolve(context.Background(), ResolveRequest{ID: "101", ReleaseOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if target.LeaseID != "cbx_cluster_a" || target.Server.CloudID != "101" {
		t.Fatalf("target=%#v, want current-cluster claim", target)
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
