package tensorlake

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	api := get("--api-url", defaultAPIURL)
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
			if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
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
	if err := b.Stop(t.Context(), StopRequest{ID: claim.LeaseID}); err == nil {
		t.Fatal("unconfirmed termination succeeded")
	}
	assertTensorlakeClaimUnchanged(t, claim)
	confirmFails = false
	r.calls = nil
	if err := b.Stop(t.Context(), StopRequest{ID: claim.Slug}); err != nil {
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
	result, err := b.Run(t.Context(), RunRequest{Repo: Repo{Root: t.TempDir()}, NoSync: true, Command: []string{"user-workload"}})
	if err != nil || result.Session == nil || !result.Session.Kept {
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
	if _, _, err := b.createSandbox(t.Context(), c, Repo{Root: t.TempDir()}, true, ""); err == nil {
		t.Fatal("acquisition overwrote successor")
	}
	if findCall(r, "sbx terminate") != nil {
		t.Fatal("rollback terminated successor")
	}
	assertTensorlakeClaimUnchanged(t, successor)
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
		if !containsArg(req.Args, defaultAPIURL) || !containsArg(req.Args, "default") {
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
	scope, _ := json.Marshal(tensorlakeScope{defaultAPIURL, defaultAPIURL, "org_fixture", "project_fixture", "default"})
	cfg := newTestConfig()
	cfg.Provider = providerName
	server := Server{Provider: providerName, CloudID: strings.TrimPrefix(leaseID, leasePrefix), Labels: map[string]string{"provider": providerName, "lease": leaseID, "slug": slug, "tensorlake_namespace": "sandbox_ns"}}
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
			_, _, err = b.createSandbox(ctx, c, Repo{Root: t.TempDir()}, false, "rollback-sandbox")
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
	if err := b.Stop(ctx, StopRequest{ID: claim.LeaseID}); !errors.Is(err, context.Canceled) {
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
