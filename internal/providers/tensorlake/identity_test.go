package tensorlake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func fixtureScopeReply(req core.LocalCommandRequest) scriptedReply {
	get := func(flag, fallback string) string {
		for i, arg := range req.Args {
			if arg == flag && i+1 < len(req.Args) {
				return req.Args[i+1]
			}
		}
		return fallback
	}
	api := get("--api-url", "https://api.tensorlake.ai")
	data, _ := json.Marshal(map[string]any{"endpoints": map[string]string{"cloudApi": api, "sandboxApi": api}, "apiKey": map[string]string{"organizationId": get("--organization", "org_fixture"), "projectId": get("--project", "project_fixture"), "key": "fixture-masked-prefix****"}})
	return scriptedReply{stdout: string(data)}
}

func ownedTensorlakeFixture(t *testing.T) (*tensorlakeBackend, *tensorlakeCLI, *recordingCommandRunner, core.LeaseClaim) {
	t.Helper()
	testutil.IsolateUserDirs(t)
	id := "3pryjysezwsnlex226i5h"
	leaseID := leasePrefix + id
	if err := claimBoundTensorlakeForTest(leaseID, "owned-sandbox", t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	claim, exists, err := core.ReadLeaseClaimWithPresence(leaseID)
	if err != nil || !exists {
		t.Fatal("fixture claim missing", err)
	}
	r := newRunner(nil, nil)
	r.resources[id] = sandboxIdentity{ID: id, Name: "crabbox-fixture", Namespace: "sandbox_ns", State: "running"}
	b := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(r)).(*tensorlakeBackend)
	c, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		t.Fatal(err)
	}
	return b, c, r, claim
}

func assertTensorlakeClaimUnchanged(t *testing.T, claim core.LeaseClaim) {
	t.Helper()
	got, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID)
	if err != nil || !exists || !reflect.DeepEqual(got, claim) {
		t.Fatal("ownership claim changed", err)
	}
}

func TestBoundStopRejectsLegacyAndMismatchedAuthority(t *testing.T) {
	for _, kind := range []string{"legacy", "wrong-cloud-id", "wrong-label", "different-project", "different-api", "different-namespace", "wrong-described-id", "duplicate-described-id", "malformed-describe", "missing-claim"} {
		t.Run(kind, func(t *testing.T) {
			b, _, r, claim := ownedTensorlakeFixture(t)
			expected := claim
			expected.Labels = map[string]string{}
			for k, v := range claim.Labels {
				expected.Labels[k] = v
			}
			switch kind {
			case "legacy":
				expected.CloudID = ""
				expected.ProviderScope = ""
				expected.Labels = nil
			case "wrong-cloud-id":
				expected.CloudID = "aaaaaaaaaaaaaaaaaaaaa"
			case "wrong-label":
				expected.Labels["lease"] = "tlsbx_aaaaaaaaaaaaaaaaaaaaa"
			case "different-api":
				b.cfg.Tensorlake.APIURL = "https://api.other.example"
			case "different-project":
				r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
					if scriptKey(req.Args) == "whoami" {
						out := strings.ReplaceAll(fixtureScopeReply(req).stdout, "project_fixture", "project_other")
						return core.LocalCommandResult{Stdout: out}, nil, true
					}
					return core.LocalCommandResult{}, nil, false
				}
			case "different-namespace":
				item := r.resources[claim.CloudID]
				item.Namespace = "other_ns"
				r.resources[claim.CloudID] = item
			case "wrong-described-id":
				r.defaults = map[string]scriptedReply{"sbx describe": {stdout: "ID: aaaaaaaaaaaaaaaaaaaaa\nNamespace: sandbox_ns\nStatus: running\n"}}
			case "duplicate-described-id":
				r.defaults = map[string]scriptedReply{"sbx describe": {stdout: "ID: " + claim.CloudID + "\nID: " + claim.CloudID + "\nNamespace: sandbox_ns\nStatus: running\n"}}
			case "malformed-describe":
				r.defaults = map[string]scriptedReply{"sbx describe": {stdout: "Status: running\n"}}
			case "missing-claim":
				if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
					t.Fatal(err)
				}
			}
			if !reflect.DeepEqual(expected, claim) {
				var err error
				expected, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, expected)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
				t.Fatal("unsafe stop succeeded")
			}
			if len(callMutationVerbs(r)) != 0 {
				t.Fatal("native mutation occurred")
			}
			if kind != "missing-claim" {
				assertTensorlakeClaimUnchanged(t, expected)
			}
		})
	}
}

func TestStopConfirmsExactTerminationAndCanRetry(t *testing.T) {
	b, _, r, claim := ownedTensorlakeFixture(t)
	confirmFails := true
	r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if scriptKey(req.Args) == "sbx describe" && r.resources[claim.CloudID].State == "terminated" && confirmFails {
			return core.LocalCommandResult{Stdout: "partial"}, errors.New("transient confirmation failure"), true
		}
		return core.LocalCommandResult{}, nil, false
	}
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("unconfirmed termination succeeded")
	}
	assertTensorlakeClaimUnchanged(t, claim)
	confirmFails = false
	r.calls = nil
	if err := b.Stop(t.Context(), core.StopRequest{ID: claim.Slug}); err != nil {
		t.Fatal(err)
	}
	if len(callMutationVerbs(r)) != 0 {
		t.Fatal("retry terminated again")
	}
	if _, exists, err := core.ReadLeaseClaimWithPresence(claim.LeaseID); err != nil || exists {
		t.Fatal("terminated claim remains", err)
	}
}

func TestStopFencesClaimDuringTermination(t *testing.T) {
	_, c, r, claim := ownedTensorlakeFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if scriptKey(req.Args) == "sbx terminate" {
			close(entered)
			<-release
		}
		return core.LocalCommandResult{}, nil, false
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.removeBoundClaim(context.Background(), claim) }()
	<-entered
	writerStarted, writerDone := make(chan struct{}), make(chan error, 1)
	go func() { close(writerStarted); writerDone <- core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim) }()
	<-writerStarted
	select {
	case err := <-writerDone:
		close(release)
		t.Fatalf("claim changed through fence: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-writerDone; err == nil {
		t.Fatal("stale writer succeeded")
	}
}

func TestStaleCleanupRejectsBeforeNativeCalls(t *testing.T) {
	_, c, r, claim := ownedTensorlakeFixture(t)
	changed := claim
	changed.RepoRoot = t.TempDir()
	changed, err := core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.removeBoundClaim(t.Context(), claim); err == nil {
		t.Fatal("stale cleanup succeeded")
	}
	if len(r.calls) != 0 {
		t.Fatal("native calls before claim admission")
	}
	assertTensorlakeClaimUnchanged(t, changed)
}

func TestBoundCleanupDeadlineIncludesClaimWait(t *testing.T) {
	assertTensorlakeClaimWaitDeadline(t, "cleanup")
}

func TestRunClaimDeadlineIncludesClaimWait(t *testing.T) {
	for _, operation := range []string{"admission", "reclaim"} {
		t.Run(operation, func(t *testing.T) {
			assertTensorlakeClaimWaitDeadline(t, operation)
		})
	}
}

func assertTensorlakeClaimWaitDeadline(t *testing.T, operation string) {
	t.Helper()
	for _, shared := range []bool{false, true} {
		t.Run(fmt.Sprintf("shared=%t", shared), func(t *testing.T) {
			b, c, runner, claim := ownedTensorlakeFixture(t)
			repoRoot := t.TempDir()
			entered, release := make(chan struct{}), make(chan struct{})
			holderDone := make(chan error, 1)
			go func() {
				hold := func() error { close(entered); <-release; return nil }
				if shared {
					holderDone <- core.WithLeaseClaimUnchangedShared(t.Context(), claim.LeaseID, claim, hold)
				} else {
					holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(*core.LeaseClaim, bool, func() error) error { return hold() })
				}
			}()
			<-entered
			ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			var result core.RunResult
			go func() {
				var err error
				switch operation {
				case "admission":
					result, err = b.Run(ctx, core.RunRequest{ID: claim.LeaseID, NoSync: true, Command: []string{"user-workload"}})
				case "reclaim":
					_, err = b.resolveLease(ctx, c, claim.LeaseID, repoRoot, true)
				default:
					err = c.removeBoundClaim(ctx, claim)
				}
				done <- err
			}()
			var err error
			returnedBeforeRelease := false
			select {
			case err = <-done:
				returnedBeforeRelease = true
			case <-time.After(time.Second):
			}
			// Join both operations even on the broken contextless baseline.
			close(release)
			if holderErr := <-holderDone; holderErr != nil {
				t.Fatal(holderErr)
			}
			if !returnedBeforeRelease {
				err = <-done
			}
			if !returnedBeforeRelease || !errors.Is(err, context.DeadlineExceeded) || len(runner.calls) != 0 {
				t.Fatalf("%s waited past deadline: returned=%t err=%v native_calls=%d", operation, returnedBeforeRelease, err, len(runner.calls))
			}
			if operation == "admission" && (result.Session == nil || !result.Session.Kept || !result.Session.Reused) {
				t.Fatalf("canceled admission lost its reused session: %+v", result.Session)
			}
			assertTensorlakeClaimUnchanged(t, claim)
			t.Logf("CLAIM_WAIT operation=%s shared=%t returned_before_release=%t deadline=%t native_calls=%d claim_unchanged=true", operation, shared, returnedBeforeRelease, errors.Is(err, context.DeadlineExceeded), len(runner.calls))
		})
	}
}

func TestOneShotTeardownKeepsOriginalClaim(t *testing.T) {
	testutil.IsolateUserDirs(t)
	id := "3pryjysezwsnlex226i5h"
	r := newRunner(map[string]scriptedReply{"sbx create": {stdout: id + "\n"}}, nil)
	var successor core.LeaseClaim
	r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if scriptKey(req.Args) == "sbx exec" && containsArg(req.Args, "user-workload") {
			claim, _, err := core.ReadLeaseClaimWithPresence(leasePrefix + id)
			if err != nil {
				t.Fatal(err)
			}
			successor = claim
			successor.RepoRoot = t.TempDir()
			successor, err = core.ReplaceLeaseClaimIfUnchangedDurableReturning(claim.LeaseID, claim, successor)
			if err != nil {
				t.Fatal(err)
			}
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(r)).(*tensorlakeBackend)
	result, err := b.Run(t.Context(), core.RunRequest{Repo: core.Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"user-workload"}})
	if err == nil || result.ExitCode != 1 || result.ErrorKind != core.RunErrorProvider || result.Session == nil || !result.Session.Kept {
		t.Fatal("expected retained session", err)
	}
	if findCall(r, "sbx terminate") != nil {
		t.Fatal("teardown terminated after claim replacement")
	}
	assertTensorlakeClaimUnchanged(t, successor)
}

func TestRollbackRetainsAppearingClaim(t *testing.T) {
	testutil.IsolateUserDirs(t)
	id := "3pryjysezwsnlex226i5h"
	r := newRunner(map[string]scriptedReply{"sbx create": {stdout: id + "\n"}}, nil)
	var successor core.LeaseClaim
	r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
		if scriptKey(req.Args) == "sbx describe" && successor.LeaseID == "" {
			if err := core.ClaimLeaseForRepoProvider(leasePrefix+id, "successor", providerName, t.TempDir(), time.Hour, false); err != nil {
				t.Fatal(err)
			}
			var err error
			successor, _, err = core.ReadLeaseClaimWithPresence(leasePrefix + id)
			if err != nil {
				t.Fatal(err)
			}
		}
		return core.LocalCommandResult{}, nil, false
	}
	b := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(r)).(*tensorlakeBackend)
	c, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.createSandbox(t.Context(), c, core.Repo{Root: t.TempDir()}, true, ""); err == nil {
		t.Fatal("acquisition overwrote successor")
	}
	if findCall(r, "sbx terminate") != nil {
		t.Fatal("rollback terminated successor")
	}
	assertTensorlakeClaimUnchanged(t, successor)
}

func TestCreatePublicationCancellationRetainsDeadlineAndSuccessor(t *testing.T) {
	b, c, runner, claim := ownedTensorlakeFixture(t)
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}
	runner.defaults = map[string]scriptedReply{"sbx create": {stdout: claim.CloudID + "\n"}}
	repo := core.Repo{Root: t.TempDir()}
	entered, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	var successor core.LeaseClaim
	go func() {
		holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(current *core.LeaseClaim, exists bool, persist func() error) error {
			if exists {
				return fmt.Errorf("expected an absent claim before creation")
			}
			close(entered)
			<-release
			*current = claim
			if err := persist(); err != nil {
				return err
			}
			successor = *current
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 250*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := b.createSandbox(ctx, c, repo, false, "")
		done <- err
	}()
	<-ctx.Done()
	close(release)
	holderErr := <-holderDone
	err := <-done
	if holderErr != nil {
		t.Fatal(holderErr)
	}
	counts := map[string]int{}
	for _, call := range runner.calls {
		counts[scriptKey(call.Args)]++
	}
	if counts["sbx create"] != 1 || counts["sbx describe"] != 1 || counts["sbx terminate"] != 0 {
		t.Fatalf("unexpected native operations: %v", counts)
	}
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("publication lost its deadline or rollback refusal: %v", err)
	}
	assertTensorlakeClaimUnchanged(t, successor)
	t.Logf("CREATE_PUBLICATION deadline=%t rollback_refused=true creates=%d describes=%d terminations=%d successor_unchanged=true", errors.Is(err, context.DeadlineExceeded), counts["sbx create"], counts["sbx describe"], counts["sbx terminate"])
}

func TestCreateRollbackDefaultBudgetIncludesClaimWait(t *testing.T) {
	b, c, runner, claim := ownedTensorlakeFixture(t)
	if err := core.RemoveLeaseClaimIfUnchanged(claim.LeaseID, claim); err != nil {
		t.Fatal(err)
	}
	stateDir, err := core.CrabboxStateDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "claims", "cbx_corrupt.json"), []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.defaults = map[string]scriptedReply{"sbx create": {stdout: claim.CloudID + "\n"}}
	repo := core.Repo{Root: t.TempDir()}
	entered, release := make(chan struct{}), make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- core.WithDurableLeaseClaimLockContext(t.Context(), claim.LeaseID, func(_ *core.LeaseClaim, exists bool, _ func() error) error {
			if exists {
				return fmt.Errorf("expected absent rollback claim")
			}
			close(entered)
			<-release
			return nil
		})
	}()
	select {
	case <-entered:
	case err := <-holderDone:
		t.Fatal(err)
	}
	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, _, err := b.createSandbox(t.Context(), c, repo, false, "rollback-budget")
		done <- err
	}()
	const expectedBudget = 30 * time.Second
	returnedBeforeRelease := false
	select {
	case err = <-done:
		returnedBeforeRelease = true
	case <-time.After(expectedBudget + 2*time.Second):
	}
	elapsed := time.Since(started)
	close(release)
	holderErr := <-holderDone
	if !returnedBeforeRelease {
		err = <-done
	}
	if holderErr != nil {
		t.Fatal(holderErr)
	}
	if !returnedBeforeRelease || elapsed < expectedBudget-time.Second || !errors.Is(err, context.DeadlineExceeded) || findCall(runner, "sbx terminate") != nil {
		t.Fatalf("rollback exceeded its fence budget: returned=%t elapsed=%s err=%v", returnedBeforeRelease, elapsed, err)
	}
	counts := map[string]int{}
	for _, call := range runner.calls {
		counts[scriptKey(call.Args)]++
	}
	if counts["sbx create"] != 1 || counts["sbx describe"] != 1 || counts["whoami"] != 1 || !strings.Contains(err.Error(), "cbx_corrupt.json") {
		t.Fatalf("rollback did not preserve its post-creation cause and original binding: calls=%v err=%v", counts, err)
	}
	if _, exists, readErr := core.ReadLeaseClaimWithPresence(claim.LeaseID); readErr != nil || exists {
		t.Fatalf("rollback published a claim: exists=%t err=%v", exists, readErr)
	}
	t.Logf("default rollback budget elapsed=%s before_release=%t native_termination=false", elapsed, returnedBeforeRelease)
}

func TestScopeProbeNeverEmitsCredentialPrefixes(t *testing.T) {
	const sentinel = "private-fixture-credential-prefix"
	for _, kind := range []string{"nonzero", "native-error", "malformed", "missing-fields", "overflow"} {
		t.Run(kind, func(t *testing.T) {
			r := newRunner(nil, nil)
			r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				result := core.LocalCommandResult{Stdout: sentinel, Stderr: sentinel}
				var err error
				switch kind {
				case "nonzero":
					result.ExitCode = 2
				case "native-error":
					err = fmt.Errorf("%s", sentinel)
				case "missing-fields":
					result.Stdout = `{"apiKey":{"key":"` + sentinel + `"}}`
				case "overflow":
					result.Stdout = strings.Repeat(sentinel, 40000)
				}
				return result, err, true
			}
			var out bytes.Buffer
			rt := newTestRuntime(r)
			rt.Stdout = &out
			rt.Stderr = &out
			c, err := newTensorlakeCLI(newTestConfig(), rt)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.observeScope(t.Context()); err == nil || strings.Contains(err.Error(), sentinel) || out.Len() != 0 {
				t.Fatal("scope probe leaked or accepted malformed evidence")
			}
		})
	}
}

func TestScopePinsRoutingAndAllowsSameProjectKeyRotation(t *testing.T) {
	b, c, r, claim := ownedTensorlakeFixture(t)
	t.Setenv("TENSORLAKE_PAT", "ignored-fixture-pat")
	t.Setenv("TENSORLAKE_DEBUG", "true")
	t.Setenv("TENSORLAKE_GIT_TOKEN", "ignored-fixture-git-token")
	t.Setenv("INDEXIFY_NAMESPACE", "hidden-namespace")
	t.Setenv("TENSORLAKE_API_URL", "https://hidden.example")
	b.cfg.Tensorlake.APIKey = "rotated-fixture-api-key"
	c, err := newTensorlakeCLI(b.cfg, b.rt)
	if err != nil {
		t.Fatal(err)
	}
	_, binding, err := bindingForClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.verifyBinding(t.Context(), binding); err != nil {
		t.Fatal(err)
	}
	for _, req := range r.calls {
		if !containsArg(req.Args, "https://api.tensorlake.ai") || !containsArg(req.Args, "default") {
			t.Fatal("scope flags were omitted")
		}
		for _, entry := range req.Env {
			if strings.Contains(entry, "ignored-fixture") || strings.HasPrefix(entry, "TENSORLAKE_DEBUG=") || strings.HasPrefix(entry, "INDEXIFY_NAMESPACE=") {
				t.Fatal("inherited override reached native CLI")
			}
		}
		if !containsEnv(req.Env, "TENSORLAKE_API_KEY=rotated-fixture-api-key") {
			t.Fatal("explicit rotated key missing")
		}
	}
}

func claimBoundTensorlakeForTest(leaseID, slug, repo string, idle time.Duration, reclaim bool) error {
	scope, _ := json.Marshal(tensorlakeScope{"https://api.tensorlake.ai", "https://api.tensorlake.ai", "org_fixture", "project_fixture", "default"})
	cfg := newTestConfig()
	cfg.Provider = providerName
	server := core.Server{Provider: providerName, CloudID: strings.TrimPrefix(leaseID, leasePrefix), Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "tensorlake_namespace": "sandbox_ns"}}
	_, err := core.ClaimLeaseTargetForRepoConfigScopeIfUnchangedDurable(leaseID, slug, cfg, string(scope), server, core.SSHTarget{}, repo, idle, reclaim, core.LeaseClaim{}, false)
	return err
}

func resolveTestLease(id, repo string, reclaim bool, idle time.Duration) (string, string, string, error) {
	cfg := newTestConfig()
	cfg.IdleTimeout = idle
	rt := newTestRuntime(newRunner(nil, nil))
	b := NewTensorlakeBackend(Provider{}.Spec(), cfg, rt).(*tensorlakeBackend)
	cli, err := newTensorlakeCLI(cfg, rt)
	if err != nil {
		return "", "", "", err
	}
	claim, err := b.resolveLease(context.Background(), cli, id, repo, reclaim)
	return claim.LeaseID, claim.CloudID, claim.Slug, err
}

func TestAcquisitionRollbackRequiresFreshScopeAndConfirmsTermination(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(fmt.Sprintf("scope-drift-%t", drift), func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			id := "3pryjysezwsnlex226i5h"
			r := newRunner(map[string]scriptedReply{"sbx create": {stdout: id + "\n"}}, nil)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			probes := 0
			r.hook = func(req core.LocalCommandRequest) (core.LocalCommandResult, error, bool) {
				if scriptKey(req.Args) != "whoami" {
					return core.LocalCommandResult{}, nil, false
				}
				probes++
				if probes == 2 {
					cancel() // Publication fails after create, before any durable claim.
				}
				if probes >= 3 && drift {
					out := strings.ReplaceAll(fixtureScopeReply(req).stdout, "project_fixture", "project_other")
					return core.LocalCommandResult{Stdout: out}, nil, true
				}
				return core.LocalCommandResult{}, nil, false
			}
			b := NewTensorlakeBackend(Provider{}.Spec(), newTestConfig(), newTestRuntime(r)).(*tensorlakeBackend)
			c, err := newTensorlakeCLI(b.cfg, b.rt)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = b.createSandbox(ctx, c, core.Repo{Root: t.TempDir()}, false, "rollback-sandbox")
			if err == nil || !errors.Is(err, context.Canceled) {
				t.Fatal("publication cancellation not preserved", err)
			}
			if _, exists, err := core.ReadLeaseClaimWithPresence(leasePrefix + id); err != nil || exists {
				t.Fatal("failed publication left a claim", err)
			}
			if drift {
				if findCall(r, "sbx terminate") != nil || !strings.Contains(err.Error(), "retained Tensorlake sandbox="+id) {
					t.Fatal("rollback did not retain changed scope")
				}
			} else if findCall(r, "sbx terminate") == nil || r.resources[id].State != "terminated" || probes < 4 {
				t.Fatal("owned rollback did not terminate and confirm")
			}
		})
	}
}

func TestScopeRejectsExplicitSelectorsThatDisagreeWithAPIKey(t *testing.T) {
	for _, selector := range []string{"organization", "project"} {
		t.Run(selector, func(t *testing.T) {
			r := newRunner(map[string]scriptedReply{"whoami": fixtureScopeReply(core.LocalCommandRequest{})}, nil)
			cfg := newTestConfig()
			if selector == "organization" {
				cfg.Tensorlake.OrganizationID = "other_org"
			} else {
				cfg.Tensorlake.ProjectID = "other_project"
			}
			c, err := newTensorlakeCLI(cfg, newTestRuntime(r))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.observeScope(t.Context()); err == nil {
				t.Fatal("explicit selector replaced API-key scope")
			}
		})
	}
}

func TestCancelledStopDoesNotInvokeNativeCLI(t *testing.T) {
	b, _, r, claim := ownedTensorlakeFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := b.Stop(ctx, core.StopRequest{ID: claim.LeaseID}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation not preserved", err)
	}
	if len(r.calls) != 0 {
		t.Fatal("cancelled stop invoked native CLI")
	}
	assertTensorlakeClaimUnchanged(t, claim)
}

func TestCanonicalTensorlakeURL(t *testing.T) {
	for _, item := range []struct{ input, want string }{
		{"https://API.EXAMPLE.COM/", "https://api.example.com"},
		{"http://127.0.0.1:9080/", "http://127.0.0.1:9080"},
		{"http://self-hosted.example/base/", "http://self-hosted.example/base"},
		{"https://user:password@example.com", ""},
		{"https://example.com?route=other", ""},
		{"https://example.com/a%2Fb", ""},
		{"file:///tmp/api", ""},
	} {
		got, err := canonicalTensorlakeURL(item.input)
		if item.want == "" {
			if err == nil {
				t.Error("ambiguous endpoint accepted")
			}
		} else if err != nil || got != item.want {
			t.Errorf("endpoint normalization got %q want %q: %v", got, item.want, err)
		}
	}
}
