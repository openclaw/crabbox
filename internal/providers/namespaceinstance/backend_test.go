package namespaceinstance

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAcquireCreatesInstanceWaitsForSSHTargetAndClaims(t *testing.T) {
	oldNewLeaseID := newLeaseID
	oldEnsureKey := ensureTestboxKeyForConfig
	oldWait := waitForSSHReady
	oldClaim := claimLeaseTargetForConfig
	defer func() {
		newLeaseID = oldNewLeaseID
		ensureTestboxKeyForConfig = oldEnsureKey
		waitForSSHReady = oldWait
		claimLeaseTargetForConfig = oldClaim
	}()
	newLeaseID = func() string { return "cbx_test001" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 synthetic", nil
	}
	waitCalled := false
	waitForSSHReady = func(_ context.Context, target *SSHTarget, _ io.Writer, phase string, _ time.Duration) error {
		waitCalled = true
		if phase != "bootstrap" || target.Host != "203.0.113.10" || target.Key != "/tmp/crabbox-test-key" {
			t.Fatalf("wait target=%#v phase=%q", target, phase)
		}
		return nil
	}
	var claimed struct {
		leaseID string
		slug    string
		server  Server
		target  SSHTarget
	}
	claimLeaseTargetForConfig = func(leaseID, slug string, _ Config, server Server, target SSHTarget, _ time.Duration) error {
		claimed.leaseID = leaseID
		claimed.slug = slug
		claimed.server = server
		claimed.target = target
		return nil
	}
	runner := &recordingRunner{results: []LocalCommandResult{
		{Stdout: `[]`},
		{Stdout: `{"id":"inst-synthetic","status":"running","ssh":{"host":"203.0.113.10","user":"root","port":22},"labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_test001","slug":"blue"}}`},
		{Stdout: `{"id":"inst-synthetic","status":"running","ssh":{"host":"203.0.113.10","user":"root","port":22}}`},
	}}
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), core.Config{
		Provider: providerName,
		TTL:      2 * time.Hour,
		NamespaceInstance: core.NamespaceInstanceConfig{
			MachineType: "linux-small",
			Duration:    90 * time.Minute,
			Ephemeral:   true,
			Region:      "us-test",
			WorkRoot:    defaultWorkRoot,
		},
	}, core.Runtime{Exec: runner, Stderr: &stderr})
	lease, err := b.Acquire(context.Background(), AcquireRequest{Keep: true, RequestedSlug: "blue"})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_test001" || lease.Server.CloudID != "inst-synthetic" || lease.SSH.Host != "203.0.113.10" {
		t.Fatalf("lease=%#v", lease)
	}
	if !waitCalled || claimed.leaseID != "cbx_test001" || claimed.slug != "blue" || claimed.server.Labels["state"] != "ready" || claimed.target.Host != "203.0.113.10" {
		t.Fatalf("wait=%v claimed=%#v", waitCalled, claimed)
	}
	joined := joinCalls(runner.calls)
	for _, want := range []string{"list -o json --all", "create", "--machine_type linux-small", "--duration 1h30m0s", "--ephemeral", "--ssh_key /tmp/crabbox-test-key.pub", "--unique_tag crabbox-cbx-test001", "describe inst-synthetic -o json"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("calls missing %q:\n%s", want, joined)
		}
	}
}

func TestAcquireMissingSSHMetadataReturnsPlanGapAndDestroysCreatedInstance(t *testing.T) {
	oldNewLeaseID := newLeaseID
	oldEnsureKey := ensureTestboxKeyForConfig
	oldWait := waitForSSHReady
	defer func() {
		newLeaseID = oldNewLeaseID
		ensureTestboxKeyForConfig = oldEnsureKey
		waitForSSHReady = oldWait
	}()
	newLeaseID = func() string { return "cbx_test002" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 synthetic", nil
	}
	waitForSSHReady = func(context.Context, *SSHTarget, io.Writer, string, time.Duration) error {
		t.Fatal("wait should not be called without SSH metadata")
		return nil
	}
	runner := &recordingRunner{results: []LocalCommandResult{
		{Stdout: `[]`},
		{Stdout: `{"id":"inst-no-ssh","labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_test002","slug":"no-ssh"}}`},
		{Stdout: `{"id":"inst-no-ssh"}`},
		{},
	}}
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{MachineType: "linux-small", WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &bytes.Buffer{}})
	_, err := b.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "plan_gap") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(joinCalls(runner.calls), "destroy inst-no-ssh --force") {
		t.Fatalf("rollback destroy not called:\n%s", joinCalls(runner.calls))
	}
}

func TestAcquireDestroysCIDFileInstanceWhenCreateReturnsError(t *testing.T) {
	oldNewLeaseID := newLeaseID
	oldEnsureKey := ensureTestboxKeyForConfig
	defer func() {
		newLeaseID = oldNewLeaseID
		ensureTestboxKeyForConfig = oldEnsureKey
	}()
	newLeaseID = func() string { return "cbx_test003" }
	ensureTestboxKeyForConfig = func(Config, string) (string, string, error) {
		return "/tmp/crabbox-test-key", "ssh-ed25519 synthetic", nil
	}
	runner := &createErrorArtifactRunner{createID: "inst-created-before-error"}
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{MachineType: "linux-small", WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &stderr})
	_, err := b.Acquire(context.Background(), AcquireRequest{})
	if err == nil || !strings.Contains(err.Error(), "nsc create failed") {
		t.Fatalf("err=%v", err)
	}
	if !strings.Contains(joinCalls(runner.calls), "destroy inst-created-before-error --force") {
		t.Fatalf("create-error rollback destroy not called:\n%s", joinCalls(runner.calls))
	}
}

func TestResolveUsesClaimAndDoesNotRequireSSHForStatusOnly(t *testing.T) {
	oldResolve := resolveLeaseClaimForProvider
	oldResolveCloud := resolveLeaseClaimForProviderCloudID
	defer func() {
		resolveLeaseClaimForProvider = oldResolve
		resolveLeaseClaimForProviderCloudID = oldResolveCloud
	}()
	resolveLeaseClaimForProvider = func(identifier, provider string) (LeaseClaim, bool, error) {
		if identifier != "blue" || provider != providerName {
			return LeaseClaim{}, false, nil
		}
		return LeaseClaim{
			LeaseID:  "cbx_claim",
			Slug:     "blue",
			Provider: providerName,
			CloudID:  "inst-claim",
			SSHHost:  "203.0.113.20",
			SSHPort:  2202,
			Labels:   map[string]string{"crabbox": "true", "provider": providerName, "lease": "cbx_claim", "slug": "blue", "state": "ready"},
		}, true, nil
	}
	resolveLeaseClaimForProviderCloudID = func(string, string) (LeaseClaim, bool, error) {
		return LeaseClaim{}, false, nil
	}
	runner := &recordingRunner{results: []LocalCommandResult{{Stdout: `{"id":"inst-claim","status":"running","labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_claim","slug":"blue"}}`}}}
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &bytes.Buffer{}})
	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "blue", StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID != "cbx_claim" || lease.Server.CloudID != "inst-claim" || lease.SSH.Host != "203.0.113.20" || lease.SSH.Port != "2202" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestListFiltersNonCrabboxInstancesByDefault(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{{Stdout: `[
		{"id":"owned","labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_owned","slug":"owned"}},
		{"id":"foreign","labels":{"project":"manual"}}
	]`}}}
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &bytes.Buffer{}})
	leases, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(leases) != 1 || leases[0].CloudID != "owned" {
		t.Fatalf("leases=%#v", leases)
	}
}

func TestReleaseDestroysClaimedInstanceAndRemovesLocalState(t *testing.T) {
	oldResolve := resolveLeaseClaimForProvider
	oldRemoveClaim := removeLeaseClaim
	oldRemoveKey := removeStoredTestboxKey
	defer func() {
		resolveLeaseClaimForProvider = oldResolve
		removeLeaseClaim = oldRemoveClaim
		removeStoredTestboxKey = oldRemoveKey
	}()
	resolveLeaseClaimForProvider = func(identifier, provider string) (LeaseClaim, bool, error) {
		if identifier != "cbx_release" || provider != providerName {
			return LeaseClaim{}, false, nil
		}
		return LeaseClaim{LeaseID: "cbx_release", Provider: providerName, CloudID: "inst-release"}, true, nil
	}
	var removedClaim, removedKey string
	removeLeaseClaim = func(leaseID string) { removedClaim = leaseID }
	removeStoredTestboxKey = func(leaseID string) { removedKey = leaseID }
	runner := &recordingRunner{results: []LocalCommandResult{{}}}
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &bytes.Buffer{}})
	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_release", Server: Server{CloudID: "inst-release"}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(joinCalls(runner.calls), "destroy inst-release --force") || removedClaim != "cbx_release" || removedKey != "cbx_release" {
		t.Fatalf("calls=%s removedClaim=%q removedKey=%q", joinCalls(runner.calls), removedClaim, removedKey)
	}
}

func TestCleanupDryRunSkipsForeignAndDoesNotDestroy(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{{Stdout: `[
		{"id":"owned","status":"failed","labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_owned","slug":"owned","state":"failed"}},
		{"id":"foreign","status":"failed","labels":{"crabbox":"true","provider":"namespace-devbox","lease":"cbx_foreign","state":"failed"}}
	]`}}}
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &stderr})
	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(joinCalls(runner.calls), "destroy") {
		t.Fatalf("dry-run destroyed: %s", joinCalls(runner.calls))
	}
	if !strings.Contains(stderr.String(), "delete server id=owned") || strings.Contains(stderr.String(), "delete server id=foreign") {
		t.Fatalf("stderr=%s", stderr.String())
	}
}

func TestCleanupRequiresRawOwnershipLabelsBeforeDestroy(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{
		{Stdout: `[
			{"id":"owned","status":"failed","labels":{"crabbox":"true","provider":"namespace-instance","lease":"cbx_owned","slug":"owned","state":"failed"}},
			{"id":"lease-only","status":"failed","labels":{"lease":"cbx_manual","slug":"manual","state":"failed"}},
			{"id":"missing-provider","status":"failed","labels":{"crabbox":"true","lease":"cbx_missing_provider","slug":"missing-provider","state":"failed"}},
			{"id":"missing-lease","status":"failed","labels":{"crabbox":"true","provider":"namespace-instance","slug":"missing-lease","state":"failed"}}
		]`},
		{},
	}}
	var stderr bytes.Buffer
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &stderr})
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	calls := joinCalls(runner.calls)
	if !strings.Contains(calls, "destroy owned --force") {
		t.Fatalf("owned instance not destroyed:\n%s", calls)
	}
	for _, id := range []string{"lease-only", "missing-provider", "missing-lease"} {
		if strings.Contains(calls, "destroy "+id+" --force") || strings.Contains(stderr.String(), "delete server id="+id) {
			t.Fatalf("foreign/partial instance %q was eligible for cleanup:\ncalls=%s\nstderr=%s", id, calls, stderr.String())
		}
		if !strings.Contains(stderr.String(), "skip server id="+id) {
			t.Fatalf("foreign/partial instance %q was not reported as skipped:\n%s", id, stderr.String())
		}
	}
}

func TestTouchExtendsInstanceAndPreservesLabels(t *testing.T) {
	runner := &recordingRunner{results: []LocalCommandResult{{}}}
	b := newBackend(Provider{}.Spec(), core.Config{Provider: providerName, NamespaceInstance: core.NamespaceInstanceConfig{Duration: time.Hour, WorkRoot: defaultWorkRoot}}, core.Runtime{Exec: runner, Stderr: &bytes.Buffer{}})
	server, err := b.Touch(context.Background(), TouchRequest{
		Lease: LeaseTarget{Server: Server{CloudID: "inst-touch", Labels: map[string]string{"crabbox": "true", "provider": providerName, "lease": "cbx_touch", "slug": "touch", "custom": "keep"}}},
		State: "ready",
	})
	if err != nil {
		t.Fatal(err)
	}
	if server.Labels["custom"] != "keep" || server.Labels["state"] != "ready" {
		t.Fatalf("labels=%#v", server.Labels)
	}
	if !strings.Contains(joinCalls(runner.calls), "extend inst-touch --ensure_minimum 1h0m0s") {
		t.Fatalf("calls=%s", joinCalls(runner.calls))
	}
}

func joinCalls(calls []LocalCommandRequest) string {
	var lines []string
	for _, call := range calls {
		lines = append(lines, call.Name+" "+strings.Join(call.Args, " "))
	}
	return strings.Join(lines, "\n")
}
