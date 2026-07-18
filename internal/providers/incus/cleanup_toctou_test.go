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
	testCleanupPreservesInstanceReclaimedDuringList(t, false, false)
}

func TestCleanupDryRunDoesNotPlanReclaimedInstanceRemoval(t *testing.T) {
	testCleanupPreservesInstanceReclaimedDuringList(t, true, false)
}

func TestCleanupPreservesInstanceReservedBeforeSnapshot(t *testing.T) {
	testCleanupPreservesInstanceReclaimedDuringList(t, false, true)
}

func testCleanupPreservesInstanceReclaimedDuringList(t *testing.T, dryRun, reserveBeforeSnapshot bool) {
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
	backdateIncusClaim(t, leaseID, now.Add(-2*time.Hour))
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

	publishFreshClaim := func() {
		freshLabels := core.TouchDirectLeaseLabels(staleLabels, core.BaseConfig(), "ready", time.Now().UTC())
		freshServer := core.Server{
			CloudID:  name,
			Provider: providerName,
			Name:     name,
			Status:   "ready",
			Labels:   freshLabels,
		}
		freshServer.Labels = cloneMap(freshLabels)
		freshServer.Labels[incusClaimReservationUntilLabel] = core.LeaseLabelTime(time.Now().UTC().Add(time.Hour))
		if err := core.ClaimLeaseForRepoProviderScopePondEndpoint(
			leaseID, slug, providerName, instanceScope(name), "", repoRoot,
			5*time.Minute, true, freshServer, core.SSHTarget{},
		); err != nil {
			t.Fatalf("reclaim during ListInstances: %v", err)
		}
	}
	if reserveBeforeSnapshot {
		publishFreshClaim()
	}
	client := &afterListClient{fakeClient: fake}
	client.afterList = func() {
		// Match Resolve's production ordering: publish the preflight claim before
		// starting or refreshing the reused instance.
		if !reserveBeforeSnapshot {
			publishFreshClaim()
		}
		freshLabels := core.TouchDirectLeaseLabels(staleLabels, core.BaseConfig(), "ready", time.Now().UTC())
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
	wantDisposition := "changed-during-cleanup"
	if reserveBeforeSnapshot {
		wantDisposition = "claim-newer-than-instance"
	}
	// In the default case Cleanup's expectedClaim came from the pre-list stale
	// claim. The reservation published by afterList is visible only to the later
	// claim-lock CAS, which must report changed-during-cleanup.
	if !strings.Contains(out.String(), wantDisposition) {
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
	// Newer than the stale instance state, but older than the bounded Resolve
	// reservation window. An abandoned reservation must not leak the instance.
	backdateIncusClaim(t, leaseID, now.Add(-time.Hour), now.Add(-time.Minute))
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

func TestCleanupClaimGenerationGuardAgesOutLegacyReservation(t *testing.T) {
	now := time.Now().UTC()
	instanceAt := now.Add(-24 * time.Hour)
	labels := map[string]string{
		"created_at":      core.LeaseLabelTime(instanceAt),
		"last_touched_at": core.LeaseLabelTime(instanceAt),
	}
	claim := core.LeaseClaim{
		ClaimedAt:          now.Add(-14 * time.Hour).Format(time.RFC3339),
		LastUsedAt:         now.Add(-14 * time.Hour).Format(time.RFC3339),
		IdleTimeoutSeconds: int(time.Hour.Seconds()),
	}
	if !incusCleanupClaimAllowsInstanceCleanup(claim, labels, now) {
		t.Fatal("legacy reservation should age out after idle timeout plus cleanup grace")
	}
	claim.LastUsedAt = now.Add(-time.Hour).Format(time.RFC3339)
	if incusCleanupClaimAllowsInstanceCleanup(claim, labels, now) {
		t.Fatal("recent legacy reservation should remain protected")
	}
	claim.Labels = map[string]string{
		incusClaimReservationUntilLabel: core.LeaseLabelTime(now.Add(-2 * time.Hour)),
	}
	if incusCleanupClaimAllowsInstanceCleanup(claim, labels, now) {
		t.Fatal("stale reservation from an older generation must use legacy grace")
	}
}

func TestCleanupClaimGenerationGuardHonorsActiveReservation(t *testing.T) {
	now := time.Now().UTC()
	claim := core.LeaseClaim{
		ClaimedAt:  now.Add(-2 * time.Minute).Format(time.RFC3339),
		LastUsedAt: now.Add(-2 * time.Minute).Format(time.RFC3339),
		Labels: map[string]string{
			incusClaimReservationUntilLabel: core.LeaseLabelTime(now.Add(time.Hour)),
		},
	}
	labels := map[string]string{
		"created_at":      core.LeaseLabelTime(now.Add(-time.Hour)),
		"last_touched_at": core.LeaseLabelTime(now.Add(-time.Minute)),
	}
	if incusCleanupClaimAllowsInstanceCleanup(claim, labels, now) {
		t.Fatal("active reservation must veto cleanup even after instance refresh")
	}
}

func TestResolveRollsBackReservationWhenCleanupDeletedAbsentClaimInstance(t *testing.T) {
	for _, test := range []struct {
		name        string
		replacement bool
		wantError   string
	}{
		{name: "deleted", wantError: "revalidate Incus instance"},
		{name: "same-name replacement", replacement: true, wantError: "changed lease identity"},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
			t.Setenv("XDG_STATE_HOME", filepath.Join(home, ".state"))
			const (
				leaseID = "cbx_incus_absent_claim"
				name    = "crabbox-incus-absent-claim"
			)
			labels := core.DirectLeaseLabels(core.BaseConfig(), leaseID, "absent-claim", providerName, "", false, time.Now().UTC())
			config := make(map[string]string, len(labels))
			for key, value := range labels {
				config[labelKey(key)] = value
			}
			fake := &fakeClient{
				instances: map[string]*api.Instance{
					name: {Name: name, Status: "Stopped", StatusCode: api.Stopped, InstancePut: api.InstancePut{Config: config}},
				},
				states: map[string]*api.InstanceState{name: {Status: "Stopped", StatusCode: api.Stopped}},
			}
			client := &afterListClient{fakeClient: fake}
			client.afterList = func() {
				delete(fake.instances, name)
				if test.replacement {
					replacementLabels := core.DirectLeaseLabels(core.BaseConfig(), "cbx_other_lease", "other", providerName, "", false, time.Now().UTC().Add(time.Second))
					replacementConfig := make(map[string]string, len(replacementLabels))
					for key, value := range replacementLabels {
						replacementConfig[labelKey(key)] = value
					}
					fake.instances[name] = &api.Instance{Name: name, Status: "Stopped", StatusCode: api.Stopped, InstancePut: api.InstancePut{Config: replacementConfig}}
				}
			}
			oldNewClient := newClient
			newClient = func(Config) (instanceClient, error) { return client, nil }
			t.Cleanup(func() { newClient = oldNewClient })

			cfg := core.BaseConfig()
			cfg.Provider = providerName
			b := newBackend(Provider{}.Spec(), cfg, core.Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}).(*backend)
			_, err := b.Resolve(context.Background(), core.ResolveRequest{ID: name, Repo: core.Repo{Root: t.TempDir()}, Reclaim: true})
			if err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("Resolve error=%v want %q", err, test.wantError)
			}
			if claim, exists, readErr := core.ReadLeaseClaimWithPresence(leaseID); readErr != nil {
				t.Fatalf("read rolled-back claim: %v", readErr)
			} else if exists || claim.LeaseID != "" {
				t.Fatalf("reservation claim survived failed revalidation: %#v", claim)
			}
		})
	}
}

func backdateIncusClaim(t *testing.T, leaseID string, at time.Time, reservationUntil ...time.Time) {
	t.Helper()
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil {
		t.Fatalf("read claim %s: %v", leaseID, err)
	}
	if !exists {
		t.Fatalf("claim %s missing before backdate", leaseID)
	}
	stale := at.UTC().Format(time.RFC3339)
	replacement := claim
	replacement.ClaimedAt = stale
	replacement.LastUsedAt = stale
	if len(reservationUntil) > 0 {
		replacement.Labels = cloneMap(replacement.Labels)
		replacement.Labels[incusClaimReservationUntilLabel] = core.LeaseLabelTime(reservationUntil[0])
	}
	if err := core.ReplaceLeaseClaimIfUnchanged(leaseID, claim, replacement); err != nil {
		t.Fatalf("backdate claim %s: %v", leaseID, err)
	}
}
