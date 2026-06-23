package agentsandbox

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestWarmupCreatesClaimAndPersistsLocalLease(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)

	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: Repo{Root: t.TempDir()}, RequestedSlug: "feature-branch"}); err != nil {
		t.Fatal(err)
	}
	if fake.creates != 1 {
		t.Fatalf("creates=%d", fake.creates)
	}
	if !strings.Contains(backend.rt.Stdout.(*bytes.Buffer).String(), "claim=crabbox-feature-branch-") {
		t.Fatalf("stdout=%q", backend.rt.Stdout)
	}
	if !strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "warmup keeps the claim") {
		t.Fatalf("stderr=%q", backend.rt.Stderr)
	}
	if !strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "ttl expiry=") {
		t.Fatalf("stderr=%q", backend.rt.Stderr)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "feature-branch")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	if claim.Provider != providerName || claim.Slug != "feature-branch" || claim.ProviderScope != claimScope(cfg) {
		t.Fatalf("claim=%#v", claim)
	}
	if !strings.HasPrefix(claim.Labels[claimLabelClaimName], "crabbox-feature-branch-") || claim.Labels[claimLabelClaimUID] == "" || claim.Labels[claimLabelPodName] == "" {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestCreateClaimReturnsPersistedLocalLease(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	now := time.Date(2026, time.June, 13, 12, 0, 0, 123456789, time.UTC)
	backend.rt.Clock = fixedClock{now: now}
	repo := testGitRepo(t)

	leaseID, claimName, slug, ready, claim, unlock, err := backend.createClaim(context.Background(), fake, "returned-claim", repo, false)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if claim.LeaseID != leaseID || claim.Slug != slug || claim.Labels[claimLabelClaimName] != claimName {
		t.Fatalf("claim=%#v lease=%s slug=%s claimName=%s", claim, leaseID, slug, claimName)
	}
	if claim.Labels[claimLabelClaimUID] != ready.ClaimUID || claim.Labels[claimLabelWarmPool] != cfg.AgentSandbox.WarmPool {
		t.Fatalf("claim labels=%#v ready=%#v", claim.Labels, ready)
	}
	wantExpiresAt := now.Add(cfg.TTL).Truncate(time.Second).Add(time.Second).Format(time.RFC3339)
	if claim.Labels[claimLabelExpiresAt] != wantExpiresAt {
		t.Fatalf("expires=%q want=%q labels=%#v", claim.Labels[claimLabelExpiresAt], wantExpiresAt, claim.Labels)
	}
	liveClaim := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
	shutdownTime, shutdownPolicy := sandboxClaimLifecycle(liveClaim)
	if shutdownTime != wantExpiresAt || shutdownPolicy != "Retain" {
		t.Fatalf("lifecycle shutdownTime=%q shutdownPolicy=%q spec=%#v", shutdownTime, shutdownPolicy, liveClaim.Spec)
	}
}

func TestWarmupReconcilesAmbiguousCreateResults(t *testing.T) {
	for _, tc := range []struct {
		name           string
		createErr      error
		emptyCreateUID bool
	}{
		{name: "accepted then transport failed", createErr: errors.New("create response lost")},
		{name: "accepted with empty response UID", emptyCreateUID: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			fake.emptyCreateUID = tc.emptyCreateUID
			if tc.createErr != nil {
				fake.createErrs = []error{tc.createErr}
				fake.getErrs = []error{errKubernetesNotFound, errors.New("transient read failure"), nil}
			}
			backend := testBackend(cfg, fake, nil, nil)

			if err := backend.Warmup(context.Background(), WarmupRequest{Repo: testGitRepo(t), RequestedSlug: "ambiguous-create"}); err != nil {
				t.Fatal(err)
			}
			claims, err := listAgentSandboxLeaseClaims()
			if err != nil {
				t.Fatal(err)
			}
			claim, ok := claimBySlug(claims, "ambiguous-create")
			if !ok || claim.Labels[claimLabelClaimUID] == "" {
				t.Fatalf("claim=%#v claims=%#v", claim, claims)
			}
			if fake.creates != 1 {
				t.Fatalf("creates=%d want=1", fake.creates)
			}
			if !strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "reconciled agent-sandbox claim=") {
				t.Fatalf("missing reconciliation warning: %s", backend.rt.Stderr)
			}
		})
	}
}

func TestWarmupReturnsDefinitiveCreateFailureWithoutReconciliation(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.createErrs = []error{&kubernetesCreateError{err: errors.New("forbidden"), ambiguous: false}}
	backend := testBackend(cfg, fake, nil, nil)

	start := time.Now()
	err := backend.Warmup(context.Background(), WarmupRequest{Repo: testGitRepo(t), RequestedSlug: "rejected-create"})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("warmup err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("definitive create failure entered reconciliation: %s", elapsed)
	}
	if len(fake.gets) != 0 {
		t.Fatalf("definitive create failure performed reconciliation GETs: %#v", fake.gets)
	}
}

func TestAmbiguousCreateRecoveryAdoptsExactClaimUID(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	leaseID := "asbx_ambiguous_recovery"
	slug := "ambiguous-recovery"
	resourceName := claimName(leaseID, slug)
	recoveryNonce := strings.Repeat("a", 64)

	err := backend.recoverAmbiguousCreateFailure(
		leaseID,
		slug,
		repo,
		false,
		resourceName,
		"",
		recoveryNonce,
		ambiguousClaimRecoveryUnknownError{err: errors.New("create response lost")},
	)
	if err == nil || !strings.Contains(err.Error(), "local lease "+leaseID+" retained") {
		t.Fatalf("recovery err=%v", err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Labels[claimLabelClaimUID] != "" || claim.Labels[claimLabelClaimUIDPending] != "true" {
		t.Fatalf("recovery labels=%#v", claim.Labels)
	}
	created, err := fake.Create(context.Background(), sandboxClaimGVR(), cfg.AgentSandbox.Namespace, &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata: objectMeta{
			Name:        resourceName,
			Namespace:   cfg.AgentSandbox.Namespace,
			Labels:      claimLabels(leaseID, slug),
			Annotations: claimAnnotationsWithRecoveryNonce(cfg, recoveryNonce),
		},
		Spec: map[string]any{"warmPoolRef": map[string]any{"name": cfg.AgentSandbox.WarmPool}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := backend.Stop(context.Background(), StopRequest{ID: leaseID}); err != nil {
		t.Fatal(err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
	if deletedUID := strings.TrimSpace(created.Metadata.UID); deletedUID == "" {
		t.Fatal("fake claim has no UID")
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("recovery lease not removed: claim=%#v err=%v", claim, err)
	}
}

func TestAmbiguousCreateRecoveryRefusesNonceMismatch(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	leaseID := "asbx_ambiguous_mismatch"
	slug := "ambiguous-mismatch"
	resourceName := claimName(leaseID, slug)
	recoveryNonce := strings.Repeat("b", 64)

	_ = backend.recoverAmbiguousCreateFailure(
		leaseID,
		slug,
		repo,
		false,
		resourceName,
		"",
		recoveryNonce,
		ambiguousClaimRecoveryUnknownError{err: errors.New("create response lost")},
	)
	_, err := fake.Create(context.Background(), sandboxClaimGVR(), cfg.AgentSandbox.Namespace, &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata: objectMeta{
			Name:        resourceName,
			Namespace:   cfg.AgentSandbox.Namespace,
			Labels:      claimLabels(leaseID, slug),
			Annotations: claimAnnotationsWithRecoveryNonce(cfg, recoveryNonce),
		},
		Spec: map[string]any{"warmPoolRef": map[string]any{"name": cfg.AgentSandbox.WarmPool}},
	})
	if err != nil {
		t.Fatal(err)
	}
	live := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+resourceName]
	live.Metadata.Annotations[annotationRecovery] = strings.Repeat("d", 64)

	err = backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "recovery nonce changed") {
		t.Fatalf("stop err=%v", err)
	}
	if fake.deletes != 0 {
		t.Fatalf("deletes=%d want=0", fake.deletes)
	}
	claim, readErr := readLeaseClaim(leaseID)
	if readErr != nil || claim.LeaseID == "" || claim.Labels[claimLabelClaimUIDPending] != "true" {
		t.Fatalf("recovery lease not retained: claim=%#v err=%v", claim, readErr)
	}
}

func TestAmbiguousCreateDefinitiveFailureDoesNotPersistRecovery(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	leaseID := "asbx_ambiguous_absent"
	slug := "ambiguous-absent"

	cause := errors.New("claim remained absent after ambiguous create")
	err := backend.recoverAmbiguousCreateFailure(leaseID, slug, repo, false, claimName(leaseID, slug), "", strings.Repeat("c", 64), cause)
	if !errors.Is(err, cause) {
		t.Fatalf("recovery err=%v want cause=%v", err, cause)
	}
	if claim, readErr := readLeaseClaim(leaseID); readErr != nil || claim.LeaseID != "" {
		t.Fatalf("definitive failure persisted claim=%#v err=%v", claim, readErr)
	}
}

func TestListReportsPendingClaimAsNotReady(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "pending"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "pending")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	sandboxName := claim.Labels[claimLabelSandboxName]
	sandbox := fake.objects[sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+sandboxName]
	sandbox.Status.Conditions = []conditionState{{Type: "Ready", Status: "False", Reason: "Starting"}}

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range views {
		if view.Labels["slug"] == "pending" {
			if view.Status != "not-ready" && view.Labels["state"] != "not-ready" {
				t.Fatalf("pending claim view=%#v", view)
			}
			return
		}
	}
	t.Fatalf("pending claim absent from views=%#v", views)
}

func TestListAndStatusReportTerminalClaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "finished"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "finished")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	sandboxName := claim.Labels[claimLabelSandboxName]
	fake.objects[sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+sandboxName].Status.Conditions = []conditionState{{
		Type: "Finished", Status: "True", Reason: "PodFailed", Message: "exit 1",
	}}

	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var terminal LeaseView
	for _, view := range views {
		if view.Labels["slug"] == "finished" {
			terminal = view
			break
		}
	}
	if terminal.Status != "failed" || !strings.Contains(terminal.Labels["reason"], "PodFailed") {
		t.Fatalf("terminal view=%#v", terminal)
	}
	for _, wait := range []bool{false, true} {
		start := time.Now()
		status, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: wait, WaitTimeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		if status.State != "failed" || status.Ready || !strings.Contains(status.Labels["reason"], "PodFailed") {
			t.Fatalf("wait=%v status=%#v", wait, status)
		}
		if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
			t.Fatalf("wait=%v terminal status took %s", wait, elapsed)
		}
	}
}

func TestListDisplayedClaimIDResolvesForStatus(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "listed"}); err != nil {
		t.Fatal(err)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var view LeaseView
	for _, candidate := range views {
		if candidate.Labels["slug"] == "listed" {
			view = candidate
			break
		}
	}
	if !strings.HasPrefix(view.CloudID, "crabbox-listed-") {
		t.Fatalf("views=%#v", views)
	}
	status, err := backend.Status(context.Background(), StatusRequest{ID: view.CloudID})
	if err != nil {
		t.Fatal(err)
	}
	if status.ServerID != view.CloudID || !status.Ready {
		t.Fatalf("status=%#v view=%#v", status, view)
	}
}

func TestRunSyncOnlyUploadsArchiveThroughPodExecTar(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, Keep: true, SyncOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !result.SyncDelegated {
		t.Fatalf("result=%#v", result)
	}
	if fake.deletes != 0 {
		t.Fatalf("kept run deleted claim")
	}
	var tarExec *podExecRequest
	for i := range fake.execs {
		if len(fake.execs[i].Command) >= 1 && fake.execs[i].Command[0] == "tar" {
			tarExec = &fake.execs[i]
			break
		}
	}
	if tarExec == nil {
		t.Fatalf("no tar exec recorded: %#v", fake.execs)
	}
	commandText := strings.Join(tarExec.Command, " ")
	if tarExec.Pod == "" || tarExec.Namespace != cfg.AgentSandbox.Namespace || !strings.HasPrefix(commandText, "tar -xzf - -C /workspace/") {
		t.Fatalf("tar exec=%#v", *tarExec)
	}
	files := tarFiles(t, fake.execInput[len(fake.execInput)-1])
	if !files["go.mod"] || !files["pkg/example.txt"] {
		t.Fatalf("archive files=%#v", files)
	}
}

func TestRunMapsRemoteExitStatus(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.execErrs = []error{
		nil,
		testExitError{code: 42},
	}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, Keep: true, NoSync: true, TimingJSON: true, Command: []string{"false"}})
	if err == nil {
		t.Fatal("nonzero remote exit returned nil error")
	}
	if result.ExitCode != 42 {
		t.Fatalf("exit=%d err=%v", result.ExitCode, err)
	}
	if !strings.Contains(err.Error(), "exited 42") {
		t.Fatalf("err=%v", err)
	}
	report := decodeLastTimingReport(t, backend.rt.Stderr.(*bytes.Buffer).String())
	if report.RunStatus != "failed" || report.ErrorKind != "command-exit" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func TestRunKeepOnFailureHintsPreserveProviderRoute(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.Kubectl = "/opt/bin/kubectl"
	cfg.AgentSandbox.Kubeconfig = "/tmp/cluster config"
	cfg.AgentSandbox.Context = "agent context"
	cfg.AgentSandbox.Namespace = "sandbox-ns"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Container = "worker"
	cfg.AgentSandbox.Workdir = "/workspace/my app"
	fake := readyFakeClient(cfg)
	fake.execErrs = []error{nil, testExitError{code: 42}}
	backend := testBackend(cfg, fake, nil, nil)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          testGitRepo(t),
		KeepOnFailure: true,
		NoSync:        true,
		Command:       []string{"false"},
	})
	if err == nil || result.ExitCode != 42 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	output := backend.rt.Stderr.(*bytes.Buffer).String()
	for _, want := range []string{
		"rerun:",
		"stop:",
		"'--agent-sandbox-kubectl' '/opt/bin/kubectl'",
		"'--agent-sandbox-kubeconfig' '/tmp/cluster config'",
		"'--agent-sandbox-context' 'agent context'",
		"'--agent-sandbox-namespace' 'sandbox-ns'",
		"'--agent-sandbox-warm-pool' 'linux-pool'",
		"'--agent-sandbox-container' 'worker'",
		"'--agent-sandbox-workdir' '/workspace/my app'",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("output missing %q:\n%s", want, output)
		}
	}
}

func TestRunKeepOnFailureRefreshesLeaseActivity(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.execDelays = []time.Duration{0, 1100 * time.Millisecond}
	fake.execErrs = []error{nil, testExitError{code: 42}}
	backend := testBackend(cfg, fake, nil, nil)

	result, err := backend.Run(context.Background(), RunRequest{
		Repo:          testGitRepo(t),
		KeepOnFailure: true,
		NoSync:        true,
		Command:       []string{"false"},
	})
	if err == nil || result.ExitCode != 42 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	claim, resolveErr := resolveLocalClaim(result.LeaseID)
	if resolveErr != nil {
		t.Fatal(resolveErr)
	}
	if claim.LastUsedAt == claim.ClaimedAt {
		t.Fatalf("kept failed lease activity was not refreshed: %#v", claim)
	}
}

func TestRunKeepOnFailureRefreshesLeaseActivityAfterSetupFailure(t *testing.T) {
	for _, tc := range []struct {
		name   string
		noSync bool
	}{
		{name: "sync"},
		{name: "no-sync", noSync: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			fake.execDelays = []time.Duration{1100 * time.Millisecond}
			fake.execErrs = []error{errors.New("setup failed")}
			backend := testBackend(cfg, fake, nil, nil)

			result, err := backend.Run(context.Background(), RunRequest{
				Repo:          testGitRepo(t),
				KeepOnFailure: true,
				NoSync:        tc.noSync,
				Command:       []string{"true"},
			})
			if err == nil || !strings.Contains(err.Error(), "setup failed") {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			claim, resolveErr := resolveLocalClaim(result.LeaseID)
			if resolveErr != nil {
				t.Fatal(resolveErr)
			}
			if claim.LastUsedAt == claim.ClaimedAt {
				t.Fatalf("retained setup-failure lease activity was not refreshed: %#v", claim)
			}
		})
	}
}

func TestRunFailsWhenDefaultOneShotCleanupFails(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	fake.deleteErrs = []error{errors.New("delete failed")}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, NoSync: true, TimingJSON: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "delete failed") {
		t.Fatalf("expected cleanup error, got result=%#v err=%v", result, err)
	}
	if result.ExitCode != 1 {
		t.Fatalf("cleanup failure should mark nonzero result: %#v", result)
	}
	if got := backend.rt.Stderr.(*bytes.Buffer).String(); !strings.Contains(got, `"exitCode":1`) {
		t.Fatalf("timing JSON did not report cleanup failure: %s", got)
	}
	report := decodeLastTimingReport(t, backend.rt.Stderr.(*bytes.Buffer).String())
	if report.RunStatus != "failed" || report.ErrorKind != "provider-error" {
		t.Fatalf("timing outcome status=%q kind=%q", report.RunStatus, report.ErrorKind)
	}
}

func decodeLastTimingReport(t *testing.T, output string) timingReport {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var report timingReport
		if err := json.Unmarshal([]byte(line), &report); err != nil {
			t.Fatalf("timing json: %v\noutput=%s", err, output)
		}
		return report
	}
	t.Fatalf("output does not contain timing JSON: %s", output)
	return timingReport{}
}

func TestStopReportsLocalClaimRemovalFailureAfterRemoteDelete(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "remove-fails"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "remove-fails")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	backend.removeClaim = func(string, LeaseClaim) error {
		return errors.New("local remove failed")
	}

	err = backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "local remove failed") {
		t.Fatalf("stop err=%v", err)
	}
	if fake.deletes != 1 {
		t.Fatalf("remote deletes=%d want=1", fake.deletes)
	}
	if strings.Contains(backend.rt.Stderr.(*bytes.Buffer).String(), "released lease=") {
		t.Fatalf("stop reported release despite local removal failure: %s", backend.rt.Stderr)
	}
	if current, readErr := readLeaseClaim(claim.LeaseID); readErr != nil || current.LeaseID == "" {
		t.Fatalf("local claim not retained after removal failure: claim=%#v err=%v", current, readErr)
	}
}

func TestRunForwardsEnvThroughStdinNotArgv(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	secret := "super-secret-token"
	result, err := backend.Run(context.Background(), RunRequest{
		Repo:    repo,
		Keep:    true,
		NoSync:  true,
		Command: []string{"printenv", "API_TOKEN"},
		Env:     map[string]string{"API_TOKEN": secret},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if len(fake.execs) < 2 {
		t.Fatalf("execs=%#v", fake.execs)
	}
	commandExec := fake.execs[len(fake.execs)-1]
	if strings.Contains(strings.Join(commandExec.Command, " "), secret) {
		t.Fatalf("secret leaked into argv: %#v", commandExec.Command)
	}
	if len(fake.execInput) == 0 || !bytes.Contains(fake.execInput[len(fake.execInput)-1], []byte(secret)) {
		t.Fatalf("script was not streamed on stdin")
	}
}

func TestRunExistingLeaseRejectsDifferentRepoWithoutReclaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "repo-a"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "repo-a")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	_, err = backend.Run(context.Background(), RunRequest{Repo: repoB, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "use --reclaim") {
		t.Fatalf("expected repo ownership error, got %v", err)
	}
}

func TestRunExistingLeaseReclaimRefreshesNewRepo(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "reclaim-me"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "reclaim-me")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	result, err := backend.Run(context.Background(), RunRequest{Repo: repoB, ID: claim.LeaseID, Reclaim: true, Keep: true, NoSync: true, Command: []string{"true"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repoB.Root {
		t.Fatalf("repo root=%q want %q", updated.RepoRoot, repoB.Root)
	}
}

func TestRunFailedValidationDoesNotPersistReclaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repoA := testGitRepo(t)
	repoB := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repoA, RequestedSlug: "failed-reclaim"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "failed-reclaim")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName].Metadata.UID = "uid-replacement"

	_, err = backend.Run(context.Background(), RunRequest{
		Repo: repoB, ID: claim.LeaseID, Reclaim: true, Keep: true, NoSync: true, Command: []string{"true"},
	})
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("failed reclaim err=%v", err)
	}
	updated, err := readLeaseClaim(claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.RepoRoot != repoA.Root || updated.LastUsedAt != claim.LastUsedAt {
		t.Fatalf("failed reclaim mutated claim: before=%#v after=%#v", claim, updated)
	}
}

func TestRunExistingLeaseValidatesLiveClaimOwnership(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "tamper"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "tamper")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	live := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
	live.Metadata.Labels = map[string]string{labelProvider: providerName, labelLeaseID: "asbx_other"}
	_, err = backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("expected live ownership error, got %v", err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("command executed despite ownership mismatch: %#v", fake.execs)
	}
}

func TestStopRejectsReplacedClaimUID(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "replace"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "replace")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName].Metadata.UID = "uid-replacement"

	err = backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	if err == nil || !strings.Contains(err.Error(), "UID changed") {
		t.Fatalf("replacement claim stop err=%v", err)
	}
	if fake.deletes != 0 {
		t.Fatalf("replacement claim was deleted: deletes=%d", fake.deletes)
	}
}

func TestStopLegacyClaimValidatesCurrentProviderScope(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	leaseID := "asbx_legacy_scope"
	slug := "legacy-scope"
	resourceName := claimName(leaseID, slug)
	uid := "uid-legacy-scope"

	if err := claimLeaseForRepoProviderScopePond(leaseID, slug, providerName, "", cfg.Pond, repo.Root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	labels := claimMetadataLabels(cfg, leaseID, sandboxReadiness{ClaimName: resourceName, ClaimUID: uid}, resourceName, "", "")
	if _, err := updateLeaseClaimLabelsIfUnchanged(leaseID, claim, labels); err != nil {
		t.Fatal(err)
	}
	_, err = fake.Create(context.Background(), sandboxClaimGVR(), cfg.AgentSandbox.Namespace, &kubernetesObject{
		APIVersion: agentSandboxExtensionsGroupVersion,
		Kind:       "SandboxClaim",
		Metadata: objectMeta{
			Name:      resourceName,
			Namespace: cfg.AgentSandbox.Namespace,
			UID:       uid,
			Labels:    claimLabels(leaseID, slug),
			Annotations: map[string]string{
				annotationScope:     scopeFingerprint("different-scope"),
				annotationWorkdir:   cfg.AgentSandbox.Workdir,
				annotationContainer: "default",
			},
		},
		Spec: map[string]any{"warmPoolRef": map[string]any{"name": cfg.AgentSandbox.WarmPool}},
	})
	if err != nil {
		t.Fatal(err)
	}

	err = backend.Stop(context.Background(), StopRequest{ID: leaseID})
	if err == nil || !strings.Contains(err.Error(), "different Crabbox scope") {
		t.Fatalf("legacy scope stop err=%v", err)
	}
	if fake.deletes != 0 {
		t.Fatalf("scope-mismatched claim was deleted: deletes=%d", fake.deletes)
	}
}

type testExitError struct {
	code int
}

type fixedClock struct {
	now time.Time
}

func (c fixedClock) Now() time.Time {
	return c.now
}

type mutableClock struct {
	now time.Time
}

func (c *mutableClock) Now() time.Time {
	return c.now
}

func (e testExitError) Error() string {
	return "remote exited"
}

func (e testExitError) ExitStatus() int {
	return e.code
}

func TestRunExistingLeaseReadinessIsBounded(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Minute
	cfg.AgentSandbox.PodReadyTimeout = time.Millisecond
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "slow"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "slow")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	podKey := cfg.AgentSandbox.Namespace + "/claim=" + claimName
	pod := fake.pods[podKey][0]
	pod.Phase = "Pending"
	pod.Ready = false
	fake.pods[podKey] = []podState{pod}
	start := time.Now()
	_, err = backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("expected bounded readiness error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readiness wait was not bounded: %s", elapsed)
	}
}

func TestClaimReadinessStopsAtPinnedTTL(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Minute
	fake := readyFakeClient(cfg)
	claim := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/claim-a"]
	claim.Status.Sandbox.Name = ""
	now := time.Date(2026, time.June, 13, 12, 0, 0, 990_000_000, time.UTC)
	expiresAt := now.Add(10 * time.Millisecond).Format(time.RFC3339)
	claim.Spec["lifecycle"] = map[string]any{"shutdownTime": expiresAt, "shutdownPolicy": "Retain"}
	backend := testBackend(cfg, fake, nil, nil)
	backend.rt.Clock = fixedClock{now: now}
	identity := fakeClaimIdentity(cfg)
	identity.ExpiresAt = expiresAt

	start := time.Now()
	_, err := backend.waitForClaimReadiness(context.Background(), fake, "claim-a", identity)
	if err == nil || !strings.Contains(err.Error(), "TTL expiry before becoming ready") {
		t.Fatalf("err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("TTL-bounded readiness took %s", elapsed)
	}
}

func TestExistingRunReleasesClaimExpiringDuringReadiness(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Minute
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	now := time.Date(2026, time.June, 13, 12, 0, 0, 990_000_000, time.UTC)
	backend.rt.Clock = fixedClock{now: now}
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "expiry-readiness"}); err != nil {
		t.Fatal(err)
	}
	claim, err := resolveLocalClaim("expiry-readiness")
	if err != nil {
		t.Fatal(err)
	}
	expiresAt := now.Add(10 * time.Millisecond).Format(time.RFC3339)
	labels := cloneStringMap(claim.Labels)
	labels[claimLabelExpiresAt] = expiresAt
	claim, err = updateLeaseClaimLabelsIfUnchanged(claim.LeaseID, claim, labels)
	if err != nil {
		t.Fatal(err)
	}
	claimName := claimNameFromLocalClaim(claim)
	liveClaim := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
	liveClaim.Spec["lifecycle"] = map[string]any{"shutdownTime": expiresAt, "shutdownPolicy": "Retain"}
	liveClaim.Status.Sandbox.Name = ""

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "TTL expiry before becoming ready") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
	if retained, readErr := readLeaseClaim(claim.LeaseID); readErr != nil || retained.LeaseID != "" {
		t.Fatalf("expired local claim retained: claim=%#v err=%v", retained, readErr)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("expired claim reached exec: %#v", fake.execs)
	}
}

func TestStatusAndListReportRetainedTTLExpiry(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	backend.rt.Clock = fixedClock{now: now}
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "expired-status"}); err != nil {
		t.Fatal(err)
	}
	claim, err := resolveLocalClaim("expired-status")
	if err != nil {
		t.Fatal(err)
	}
	backend.rt.Clock = fixedClock{now: now.Add(cfg.TTL + time.Second)}
	claimName := claimNameFromLocalClaim(claim)
	delete(fake.objects, sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+claim.Labels[claimLabelSandboxName])
	delete(fake.pods, cfg.AgentSandbox.Namespace+"/claim="+claimName)

	view, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: true, WaitTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "expired" || view.Ready || !strings.Contains(view.Labels["reason"], "TTL expired") {
		t.Fatalf("view=%#v", view)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "expired" || !strings.Contains(views[0].Labels["reason"], "TTL expired") {
		t.Fatalf("views=%#v", views)
	}
}

func TestStatusReportsControllerClaimExpiredCondition(t *testing.T) {
	for _, conditionReason := range []string{"ClaimExpired", "SandboxExpired"} {
		t.Run(conditionReason, func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			backend := testBackend(cfg, fake, nil, nil)
			repo := testGitRepo(t)
			slug := "controller-" + strings.ToLower(conditionReason)
			if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: slug}); err != nil {
				t.Fatal(err)
			}
			claim, err := resolveLocalClaim(slug)
			if err != nil {
				t.Fatal(err)
			}
			claimName := claimNameFromLocalClaim(claim)
			liveClaim := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
			liveClaim.Status.Conditions = append(liveClaim.Status.Conditions, conditionState{Type: "Ready", Status: "False", Reason: conditionReason})
			wantReason := "controller reported " + conditionReason
			if expired, reason := sandboxClaimExpired(claim, liveClaim, backend.now().UTC()); !expired || reason != wantReason {
				t.Fatalf("expired=%t reason=%q conditions=%#v", expired, reason, liveClaim.Status.Conditions)
			}

			_, runErr := backend.Run(context.Background(), RunRequest{
				Repo: repo, ID: claim.LeaseID, Keep: true, NoSync: true, Command: []string{"true"},
			})
			if runErr == nil || !strings.Contains(runErr.Error(), conditionReason) {
				t.Fatalf("run err=%v", runErr)
			}
			if len(fake.execs) != 0 {
				t.Fatalf("expired claim reached pod exec: %#v", fake.execs)
			}

			view, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: true, WaitTimeout: time.Minute})
			if err != nil {
				t.Fatal(err)
			}
			if view.State != "expired" || view.Labels["reason"] != wantReason {
				t.Fatalf("view=%#v", view)
			}
		})
	}
}

func TestStatusAndListReportDirectSandboxExpiry(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "sandbox-expired"}); err != nil {
		t.Fatal(err)
	}
	claim, err := resolveLocalClaim("sandbox-expired")
	if err != nil {
		t.Fatal(err)
	}
	sandboxName := claim.Labels[claimLabelSandboxName]
	fake.objects[sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+sandboxName].Status.Conditions = []conditionState{{
		Type: "Ready", Status: "False", Reason: "SandboxExpired", Message: "lifetime elapsed",
	}}

	start := time.Now()
	view, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: true, WaitTimeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("direct sandbox expiry was retried")
	}
	if view.State != "expired" || !strings.Contains(view.Labels["reason"], "SandboxExpired") {
		t.Fatalf("view=%#v", view)
	}
	views, err := backend.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].Status != "expired" || !strings.Contains(views[0].Labels["reason"], "SandboxExpired") {
		t.Fatalf("views=%#v", views)
	}
}

func TestWarmupRetainsRecoverableClaimWhenReadinessCleanupFails(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.SandboxReadyTimeout = time.Millisecond
	fake := readyFakeClient(cfg)
	fake.createPending = true
	fake.deleteErrs = []error{errors.New("delete failed")}
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)

	err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "recoverable"})
	if err == nil || !strings.Contains(err.Error(), "local lease") || !strings.Contains(err.Error(), "retained") {
		t.Fatalf("warmup err=%v", err)
	}
	claims, listErr := listAgentSandboxLeaseClaims()
	if listErr != nil {
		t.Fatal(listErr)
	}
	claim, ok := claimBySlug(claims, "recoverable")
	if !ok {
		t.Fatalf("recoverable claim missing: %#v", claims)
	}
	if claim.Labels[claimLabelClaimUID] == "" || claim.Labels["state"] != "not-ready" {
		t.Fatalf("recoverable claim labels=%#v", claim.Labels)
	}
	if stopErr := backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); stopErr != nil {
		t.Fatal(stopErr)
	}
	if retained, readErr := readLeaseClaim(claim.LeaseID); readErr != nil || retained.LeaseID != "" {
		t.Fatalf("recovery stop retained claim=%#v err=%v", retained, readErr)
	}
}

func TestConcurrentRequestedSlugAllocationIsAtomic(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fakeA := readyFakeClient(cfg)
	fakeA.createStarted = make(chan struct{}, 1)
	fakeA.createRelease = make(chan struct{})
	fakeB := readyFakeClient(cfg)
	fakeB.createStarted = make(chan struct{}, 1)
	backendA := testBackend(cfg, fakeA, nil, nil)
	backendB := testBackend(cfg, fakeB, nil, nil)
	repo := testGitRepo(t)
	errs := make(chan error, 2)

	go func() {
		errs <- backendA.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "shared"})
	}()
	select {
	case <-fakeA.createStarted:
	case <-time.After(time.Second):
		t.Fatal("first create did not start")
	}
	go func() {
		errs <- backendB.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "shared"})
	}()
	select {
	case <-fakeB.createStarted:
		t.Fatal("second create started before the first slug was persisted")
	case <-time.After(50 * time.Millisecond):
	}
	close(fakeA.createRelease)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].Slug == claims[1].Slug {
		t.Fatalf("claims=%#v, want two unique slugs", claims)
	}
}

func TestGeneratedSlugAllocationIsSerialized(t *testing.T) {
	testAgentSandboxConfig(t)
	unlockFirst, err := lockAgentSandboxSlugAllocation(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer unlockFirst()

	acquired := make(chan func(), 1)
	go func() {
		unlock, lockErr := lockAgentSandboxSlugAllocation(context.Background(), "")
		if lockErr == nil {
			acquired <- unlock
		}
	}()
	select {
	case unlock := <-acquired:
		unlock()
		t.Fatal("second generated-slug allocation acquired concurrently")
	case <-time.After(50 * time.Millisecond):
	}
	unlockFirst()
	unlockFirst = func() {}
	select {
	case unlock := <-acquired:
		unlock()
	case <-time.After(time.Second):
		t.Fatal("second generated-slug allocation did not proceed")
	}
}

func TestGeneratedSlugAllocationAvoidsExistingClaim(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	repo := testGitRepo(t)
	firstID := ""
	secondID := ""
	bySlug := map[string]string{}
	for i := 0; i < 1000; i++ {
		leaseID := fmt.Sprintf("isb_%012x", i)
		slug := core.NewLeaseSlug(leaseID)
		bySlug[slug] = leaseID
	}
	for i := 0; i < 1000; i++ {
		leaseID := fmt.Sprintf("asbx_%012x", i)
		if previous := bySlug[core.NewLeaseSlug(leaseID)]; previous != "" {
			firstID, secondID = previous, leaseID
			break
		}
	}
	if firstID == "" {
		t.Fatal("could not find deterministic generated-slug collision")
	}
	base := core.NewLeaseSlug(firstID)
	if err := core.ClaimLeaseForRepoProviderScope(firstID, base, "islo", "test-scope", repo.Root, cfg.IdleTimeout, false); err != nil {
		t.Fatal(err)
	}

	got, err := allocateClaimLeaseSlug(secondID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got == base {
		t.Fatalf("generated slug collision was not resolved: %q", got)
	}
}

func TestRunExistingLeaseForgetMissingStillFailsRun(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.ForgetMissing = true
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)
	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "command not run") {
		t.Fatalf("expected stale run failure, result=%#v err=%v", result, err)
	}
	forgotten, readErr := readLeaseClaim(claim.LeaseID)
	if readErr != nil || forgotten.LeaseID != "" {
		t.Fatalf("missing claim was not forgotten: claim=%#v err=%v", forgotten, readErr)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("command executed despite missing claim: %#v", fake.execs)
	}
}

func TestExpiredMissingClaimsRemainFailClosedWithoutForgetMissing(t *testing.T) {
	for _, operation := range []string{"run", "stop", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			backend := testBackend(cfg, fake, nil, nil)
			now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
			backend.rt.Clock = fixedClock{now: now}
			repo := testGitRepo(t)
			slug := "expired-missing-" + operation
			if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: slug}); err != nil {
				t.Fatal(err)
			}
			claims, err := listAgentSandboxLeaseClaims()
			if err != nil {
				t.Fatal(err)
			}
			claim, ok := claimBySlug(claims, slug)
			if !ok {
				t.Fatalf("claims=%#v", claims)
			}
			claimName := claim.Labels[claimLabelClaimName]
			delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)
			backend.rt.Clock = fixedClock{now: now.Add(cfg.TTL + time.Second)}

			switch operation {
			case "run":
				result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
				if err == nil || !strings.Contains(err.Error(), "claim retained") {
					t.Fatalf("result=%#v err=%v", result, err)
				}
			case "stop":
				if err := backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID}); err == nil || !strings.Contains(err.Error(), "claim retained") {
					t.Fatalf("err=%v", err)
				}
			case "cleanup":
				if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
					t.Fatal(err)
				}
			}
			retained, readErr := readLeaseClaim(claim.LeaseID)
			if readErr != nil || retained.LeaseID == "" {
				t.Fatalf("operation=%s claim=%#v err=%v", operation, retained, readErr)
			}
		})
	}
}

func TestRunRejectsAndReleasesExpiredLiveClaimBeforeExec(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	backend.rt.Clock = fixedClock{now: now}
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "expired-live"}); err != nil {
		t.Fatal(err)
	}
	backend.rt.Clock = fixedClock{now: now.Add(cfg.TTL + time.Second)}

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: "expired-live", NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "reached its TTL expiry") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fake.execs) != 0 {
		t.Fatalf("expired claim reached exec: %#v", fake.execs)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
	if claim, readErr := resolveLocalClaim("expired-live"); readErr == nil || claim.LeaseID != "" {
		t.Fatalf("expired local claim retained: claim=%#v err=%v", claim, readErr)
	}
}

func TestRunReportsAndReleasesClaimThatExpiresDuringCommand(t *testing.T) {
	for _, keep := range []bool{false, true} {
		t.Run(fmt.Sprintf("keep=%t", keep), func(t *testing.T) {
			cfg := testAgentSandboxConfig(t)
			fake := readyFakeClient(cfg)
			fake.execStarted = make(chan struct{}, 1)
			fake.execRelease = make(chan struct{})
			backend := testBackend(cfg, fake, nil, nil)
			now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
			clock := &mutableClock{now: now}
			backend.rt.Clock = clock
			repo := testGitRepo(t)

			type runOutcome struct {
				result RunResult
				err    error
			}
			outcome := make(chan runOutcome, 1)
			go func() {
				result, err := backend.Run(context.Background(), RunRequest{Repo: repo, Keep: keep, NoSync: true, Command: []string{"true"}})
				outcome <- runOutcome{result: result, err: err}
			}()
			<-fake.execStarted
			if keep {
				claims, err := listAgentSandboxLeaseClaims()
				if err != nil || len(claims) != 1 {
					t.Fatalf("claims=%#v err=%v", claims, err)
				}
				current := claims[0]
				labels := cloneStringMap(current.Labels)
				labels["test_activity_refresh"] = "true"
				if _, updateErr := updateLeaseClaimLabelsIfUnchanged(current.LeaseID, current, labels); updateErr != nil {
					t.Fatal(updateErr)
				}
			}
			clock.now = now.Add(cfg.TTL + time.Second)
			close(fake.execRelease)
			got := <-outcome

			result, err := got.result, got.err
			if err == nil || !strings.Contains(err.Error(), "TTL expiry during the run") {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if fake.deletes != 1 {
				t.Fatalf("deletes=%d want=1", fake.deletes)
			}
			if claim, readErr := readLeaseClaim(result.LeaseID); readErr != nil || claim.LeaseID != "" {
				t.Fatalf("expired local claim retained: claim=%#v err=%v", claim, readErr)
			}
		})
	}
}

func TestRunExistingLeaseRetainsClaimWhenOnlyDownstreamResourceIsMissing(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.ForgetMissing = true
	cfg.AgentSandbox.SandboxReadyTimeout = 20 * time.Millisecond
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "downstream-gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "downstream-gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	sandboxName := claim.Labels[claimLabelSandboxName]
	delete(fake.objects, sandboxResource+"/"+cfg.AgentSandbox.Namespace+"/"+sandboxName)

	result, err := backend.Run(context.Background(), RunRequest{Repo: repo, ID: claim.LeaseID, NoSync: true, Command: []string{"true"}})
	if err == nil || !strings.Contains(err.Error(), "readiness timed out") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	retained, readErr := readLeaseClaim(claim.LeaseID)
	if readErr != nil || retained.LeaseID == "" {
		t.Fatalf("downstream miss forgot live root claim: claim=%#v err=%v", retained, readErr)
	}
}

func TestReadinessRunErrorForgetsOnlyAfterRootClaimRecheck(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	cfg.AgentSandbox.ForgetMissing = true
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "root-gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "root-gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)

	err = backend.readinessRunError(context.Background(), fake, claim, claimName, fmt.Errorf("pod disappeared: %w", errKubernetesNotFound))
	if err == nil || !strings.Contains(err.Error(), "local claim forgotten") {
		t.Fatalf("err=%v", err)
	}
	forgotten, readErr := readLeaseClaim(claim.LeaseID)
	if readErr != nil || forgotten.LeaseID != "" {
		t.Fatalf("root claim recheck did not forget local claim: claim=%#v err=%v", forgotten, readErr)
	}
}

func TestStopSerializesWithActiveLeaseRun(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "active"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "active")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	fake.execStarted = make(chan struct{}, 1)
	fake.execRelease = make(chan struct{})

	runDone := make(chan error, 1)
	go func() {
		_, err := backend.Run(context.Background(), RunRequest{
			ID: claim.LeaseID, Repo: repo, Keep: true, NoSync: true, Command: []string{"true"},
		})
		runDone <- err
	}()
	select {
	case <-fake.execStarted:
	case <-time.After(time.Second):
		t.Fatal("run did not reach user command")
	}

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- backend.Stop(context.Background(), StopRequest{ID: claim.LeaseID})
	}()
	select {
	case err := <-stopDone:
		t.Fatalf("stop completed while run held operation lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(fake.execRelease)
	if err := <-runDone; err != nil {
		t.Fatal(err)
	}
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
}

func TestCleanupSerializesWithLeaseReuse(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	backend.rt.Clock = fixedClock{now: time.Now().Add(2 * time.Hour)}
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "idle"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "idle")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	unlock, err := lockAgentSandboxLeaseOperation(context.Background(), claim.LeaseID)
	if err != nil {
		t.Fatal(err)
	}

	cleanupDone := make(chan error, 1)
	go func() {
		cleanupDone <- backend.Cleanup(context.Background(), CleanupRequest{})
	}()
	select {
	case err := <-cleanupDone:
		unlock()
		t.Fatalf("cleanup completed while lease lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unlock()
	if err := <-cleanupDone; err != nil {
		t.Fatal(err)
	}
	if fake.deletes != 1 {
		t.Fatalf("deletes=%d want=1", fake.deletes)
	}
}

func TestCleanupValidatesClaimIdentityBeforeDryRunOrIdleSkip(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "redirected-cleanup"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "redirected-cleanup")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	liveClaim := fake.objects[sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName]
	liveClaim.Spec["warmPoolRef"].(map[string]any)["name"] = "other-pool"

	err = backend.Cleanup(context.Background(), CleanupRequest{DryRun: true})
	if err == nil || !strings.Contains(err.Error(), "warm pool changed") {
		t.Fatalf("cleanup err=%v", err)
	}
	if strings.Contains(backend.rt.Stdout.(*bytes.Buffer).String(), "would delete") {
		t.Fatalf("dry-run predicted unsafe deletion: %s", backend.rt.Stdout)
	}
}

func TestClaimCleanupDueHonorsAbsoluteTTL(t *testing.T) {
	now := time.Date(2026, time.June, 13, 12, 0, 0, 0, time.UTC)
	claim := LeaseClaim{
		LastUsedAt:         now.Format(time.RFC3339),
		IdleTimeoutSeconds: int(time.Hour.Seconds()),
		Labels: map[string]string{
			claimLabelExpiresAt: now.Add(-time.Second).Format(time.RFC3339),
		},
	}
	due, reason := claimCleanupDue(claim, now)
	if !due || reason != "ttl" {
		t.Fatalf("due=%v reason=%q", due, reason)
	}
}

func TestAgentSandboxOperationLockHonorsContextCancellation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	unlock, err := lockAgentSandboxLeaseOperation(context.Background(), leasePrefix+"lock-test")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := lockAgentSandboxLeaseOperation(ctx, leasePrefix+"lock-test"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v, want context deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancellation took %s", elapsed)
	}
}

func TestCleanupContextIgnoresCancelledParent(t *testing.T) {
	backend := &backend{}
	parent, cancelParent := context.WithCancel(context.Background())
	cancelParent()
	cleanupCtx, cancelCleanup := backend.cleanupContext(parent)
	defer cancelCleanup()
	if err := cleanupCtx.Err(); err != nil {
		t.Fatalf("cleanup context inherited cancellation: %v", err)
	}
}

func TestStatusMissingClaimDoesNotWaitAsNotReady(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-gone"}); err != nil {
		t.Fatal(err)
	}
	claims, err := listAgentSandboxLeaseClaims()
	if err != nil {
		t.Fatal(err)
	}
	claim, ok := claimBySlug(claims, "status-gone")
	if !ok {
		t.Fatalf("claims=%#v", claims)
	}
	claimName := claim.Labels[claimLabelClaimName]
	delete(fake.objects, sandboxClaimResource+"/"+cfg.AgentSandbox.Namespace+"/"+claimName)
	view, err := backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID})
	if err != nil {
		t.Fatal(err)
	}
	if view.State != "missing-or-inaccessible" {
		t.Fatalf("view=%#v", view)
	}
	start := time.Now()
	_, err = backend.Status(context.Background(), StatusRequest{ID: claim.LeaseID, Wait: true, WaitTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "missing in Kubernetes") {
		t.Fatalf("expected missing wait error, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("status waited despite missing claim: %s", elapsed)
	}
}

func TestStatusWaitReturnsWhenClaimDisappearsDuringPolling(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-disappears"}); err != nil {
		t.Fatal(err)
	}
	fake.getErrs = []error{nil, errKubernetesNotFound}

	start := time.Now()
	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-disappears", Wait: true, WaitTimeout: time.Second})
	if err == nil || !strings.Contains(err.Error(), "missing in Kubernetes") {
		t.Fatalf("status err=%v", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("status waited after root claim disappeared: %s", elapsed)
	}
}

func TestStatusWaitTimeoutBoundsInitialLookup(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-timeout"}); err != nil {
		t.Fatal(err)
	}
	fake.getStarted = make(chan struct{}, 1)
	fake.getRelease = make(chan struct{})

	start := time.Now()
	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-timeout", Wait: true, WaitTimeout: 20 * time.Millisecond})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("status err=%v, want deadline exceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("initial lookup ignored wait timeout: %s", elapsed)
	}
}

func TestStatusReturnsUnexpectedReadinessError(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)
	repo := testGitRepo(t)
	if err := backend.Warmup(context.Background(), WarmupRequest{Repo: repo, RequestedSlug: "status-error"}); err != nil {
		t.Fatal(err)
	}
	fake.getErrs = []error{nil, errors.New("transport failed")}

	_, err := backend.Status(context.Background(), StatusRequest{ID: "status-error"})
	if err == nil || !strings.Contains(err.Error(), "transport failed") {
		t.Fatalf("status err=%v", err)
	}
}

func TestCleanupRetainsMissingClaimsUnlessForgetMissing(t *testing.T) {
	cfg := testAgentSandboxConfig(t)
	repo := t.TempDir()
	leaseID := "asbx_missing"
	if err := claimLeaseForRepo(cfg, leaseID, "missing-claim", Repo{Root: repo}, false); err != nil {
		t.Fatal(err)
	}
	claim, err := readLeaseClaim(leaseID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = updateLeaseClaimLabelsIfUnchanged(leaseID, claim, map[string]string{claimLabelClaimName: "missing-claim"})
	if err != nil {
		t.Fatal(err)
	}
	fake := readyFakeClient(cfg)
	backend := testBackend(cfg, fake, nil, nil)

	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := readLeaseClaim(leaseID); err != nil {
		t.Fatalf("claim removed without forgetMissing: %v", err)
	}

	cfg.AgentSandbox.ForgetMissing = true
	backend = testBackend(cfg, fake, nil, nil)
	if err := backend.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if claim, err := readLeaseClaim(leaseID); err != nil || claim.LeaseID != "" {
		t.Fatalf("claim not forgotten: claim=%#v err=%v", claim, err)
	}
}

func testAgentSandboxConfig(t *testing.T) Config {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	cfg := core.BaseConfig()
	cfg.AgentSandbox.Context = "agent-context"
	cfg.AgentSandbox.Namespace = "sandboxes"
	cfg.AgentSandbox.WarmPool = "linux-pool"
	cfg.AgentSandbox.Workdir = "/workspace/crabbox"
	cfg.AgentSandbox.DeleteOnRelease = true
	cfg.IdleTimeout = time.Hour
	return cfg
}

func testBackend(cfg Config, fake *fakeKubernetesClient, stdout, stderr *bytes.Buffer) *backend {
	if stdout == nil {
		stdout = &bytes.Buffer{}
	}
	if stderr == nil {
		stderr = &bytes.Buffer{}
	}
	return &backend{
		spec: Provider{}.Spec(),
		cfg:  cfg,
		rt:   Runtime{Stdout: stdout, Stderr: stderr},
		newClient: func(context.Context, Config, Runtime) (kubernetesClient, error) {
			return fake, nil
		},
	}
}

func claimBySlug(claims []LeaseClaim, slug string) (LeaseClaim, bool) {
	for _, claim := range claims {
		if claim.Slug == slug {
			return claim, true
		}
	}
	return LeaseClaim{}, false
}

func testGitRepo(t *testing.T) Repo {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.test/repo\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pkg", "example.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init"}, {"add", "."}, {"commit", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return Repo{Root: root, Name: "repo"}
}

func tarFiles(t *testing.T, data []byte) map[string]bool {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	files := map[string]bool{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = true
	}
	return files
}
