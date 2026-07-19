package githubcodespaces

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestAcquireCreatesClaimGeneratesSSHConfigAndWaitsReady(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{
		fakeCodespace("cs-1", "Provisioning"),
		fakeCodespace("cs-1", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedSlug: "green-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.LeaseID == "" || lease.Server.CloudID != "cs-1" || lease.Server.Labels[labelCodespaceName] != "cs-1" {
		t.Fatalf("lease=%#v", lease)
	}
	if lease.Server.Labels[labelRepository] != "example-org/my-app" || lease.Server.Labels[labelLogin] != "alice" || lease.Server.Labels[labelRelease] != releaseDelete {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	if len(fc.creates) != 1 {
		t.Fatalf("creates=%#v", fc.creates)
	}
	create := fc.creates[0]
	if create.Repo != "example-org/my-app" || create.Ref != "main" || create.Machine != "standardLinux32gb" ||
		create.DevcontainerPath != ".devcontainer/devcontainer.json" || create.WorkingDirectory != "/workspaces/my-app" ||
		create.Geo != "UsWest" || !strings.HasPrefix(create.DisplayName, "crabbox-green-box-") {
		t.Fatalf("create=%#v", create)
	}
	if len(b.waits) != 1 {
		t.Fatalf("waits=%#v", b.waits)
	}
	wait := b.waits[0]
	if wait.User != "vscode" || wait.Host != "cs.cs-1.main" || wait.Key != "/tmp/codespaces/key" || !wait.SSHConfigProxy {
		t.Fatalf("wait target=%#v", wait)
	}
	if !strings.Contains(wait.ReadyCheck, "test -d '/workspaces/my-app'") {
		t.Fatalf("ready check=%q", wait.ReadyCheck)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.CloudID != "cs-1" || claim.SSHHost != "cs.cs-1.main" || claim.Labels[labelEnvironmentID] != "env-cs-1" || claim.Labels["work_root"] != "/workspaces/my-app" {
		t.Fatalf("claim=%#v", claim)
	}
	if fg.configFor != "cs-1" {
		t.Fatalf("ssh config generated for %q", fg.configFor)
	}
}

func TestAcquireUsesExplicitGenericServerType(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{fakeCodespace("cs-1", "Available")}
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	b.cfg.ServerType = "premiumLinux"
	b.cfg.ServerTypeExplicit = true

	if _, err := b.Acquire(context.Background(), AcquireRequest{Repo: Repo{Root: t.TempDir(), Name: "my-app"}}); err != nil {
		t.Fatal(err)
	}
	if len(fc.creates) != 1 || fc.creates[0].Machine != "premiumLinux" {
		t.Fatalf("creates=%#v", fc.creates)
	}
}

func TestAcquirePersistsRecoveryClaimBeforeCreate(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{fakeCodespace("cs-1", "Available")}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aa1"
	fc.onCreate = func(req createCodespaceRequest) {
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil || !ok {
			t.Fatalf("pre-create claim ok=%t err=%v", ok, err)
		}
		if claim.CloudID != "" || claim.ProviderScope != "repo:example-org/my-app" {
			t.Fatalf("pre-create claim=%#v", claim)
		}
		if claim.Labels[labelRecovery] != recoveryPreCreate || claim.Labels[labelDisplayName] != req.DisplayName || claim.Labels[labelRepository] != req.Repo || claim.Labels[labelLogin] != "alice" {
			t.Fatalf("pre-create labels=%#v request=%#v", claim.Labels, req)
		}
	}

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "durable-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "cs-1" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestAcquireRecoversAmbiguousCreateByExactIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.createErr = errors.New("create response lost")
	fc.onCreate = func(req createCodespaceRequest) {
		item := fakeCodespace("cs-recovered", "Available")
		item.DisplayName = req.DisplayName
		item.Repository.FullName = req.Repo
		fc.items[item.Name] = item
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	callbackCalled := false

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789aa2",
		RequestedSlug:    "recovered-box",
		OnAcquired: func(lease LeaseTarget) error {
			callbackCalled = true
			claim, ok, claimErr := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
			if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
				t.Fatalf("claim was bound before callback: claim=%#v ok=%t err=%v", claim, ok, claimErr)
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !callbackCalled || lease.Server.CloudID != "cs-recovered" || len(fc.deletes) != 0 {
		t.Fatalf("lease=%#v deletes=%#v", lease, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok || claim.CloudID != "cs-recovered" || claim.Labels[labelRecovery] != "" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestAcquireRecoversIdentitylessSuccessByExactIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.useCreateResult = true
	fc.onCreate = func(req createCodespaceRequest) {
		item := fakeCodespace("cs-identityless", "Available")
		item.DisplayName = req.DisplayName
		item.Repository.FullName = req.Repo
		fc.items[item.Name] = item
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789aa8",
		RequestedSlug:    "identityless-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.CloudID != "cs-identityless" || len(fc.deletes) != 0 {
		t.Fatalf("lease=%#v deletes=%#v", lease, fc.deletes)
	}
}

func TestAcquireRejectsIncompletePermanentIdentity(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*codespace)
		want   string
	}{
		{name: "codespace id", mutate: func(item *codespace) { item.ID = 0 }, want: "incomplete permanent resource identity"},
		{name: "environment id", mutate: func(item *codespace) { item.EnvironmentID = "" }, want: "incomplete permanent resource identity"},
		{name: "owner id", mutate: func(item *codespace) { item.Owner.ID = 0 }, want: "incomplete permanent resource identity"},
		{name: "owner login", mutate: func(item *codespace) { item.Owner.Login = "" }, want: "incomplete permanent resource identity"},
		{name: "repository id", mutate: func(item *codespace) { item.Repository.ID = 0 }, want: "incomplete permanent resource identity"},
		{name: "wrong owner", mutate: func(item *codespace) { item.Owner = fakeGitHubUser("bob") }, want: "owner mismatch"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			fc := newFakeCodespacesClient()
			fc.useCreateResult = true
			fc.createResult = fakeCodespace("cs-incomplete", "Available")
			test.mutate(&fc.createResult)
			b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
			leaseID := "cbx_123456789ab2"
			callbackCalled := false

			_, err := b.Acquire(context.Background(), AcquireRequest{
				Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
				RequestedLeaseID: leaseID,
				RequestedSlug:    "incomplete-box",
				OnAcquired: func(LeaseTarget) error {
					callbackCalled = true
					return nil
				},
			})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err=%v", err)
			}
			if callbackCalled || len(fc.deletes) != 0 {
				t.Fatalf("callback=%t deletes=%#v", callbackCalled, fc.deletes)
			}
			claim, ok, claimErr := readLeaseClaimWithPresence(leaseID)
			if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
				t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
			}
		})
	}
}

func TestAcquireAcceptsRenamedOwnerWithSameUserID(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.useCreateResult = true
	fc.createResult = fakeCodespace("cs-renamed-owner", "Available")
	fc.createResult.Owner.Login = "alice-renamed"
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "test" + "-value"})

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789af1",
		RequestedSlug:    "renamed-owner-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels[labelLogin] != "alice-renamed" {
		t.Fatalf("login=%q", lease.Server.Labels[labelLogin])
	}
	if lease.Server.Labels[labelUserID] != fmt.Sprintf("%d", fakeGitHubUser("alice").ID) {
		t.Fatalf("user id=%q", lease.Server.Labels[labelUserID])
	}
}

func TestAcquireRejectsAndRollsBackZeroEffectiveRetention(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.useCreateResult = true
	fc.createResult = fakeCodespace("cs-zero-effective-retention", "Available")
	zero := 0
	fc.createResult.RetentionPeriodMinutes = &zero
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "test" + "-value"})
	leaseID := "cbx_123456789af7"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "zero-effective-retention",
	})
	if err == nil || !strings.Contains(err.Error(), "effective retention is zero") {
		t.Fatalf("err=%v", err)
	}
	if strings.Join(fc.deletes, ",") != "cs-zero-effective-retention" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || ok {
		t.Fatalf("claim ok=%t err=%v", ok, claimErr)
	}
}

func TestValidateClaimScopeAcceptsRenamedLoginForSameUserID(t *testing.T) {
	b := newTestBackend(t, newFakeCodespacesClient(), &fakeGH{login: "alice", token: "test" + "-value"})
	user := fakeGitHubUser("alice")
	claim := LeaseClaim{
		LeaseID:       "cbx_123456789af2",
		Provider:      providerName,
		ProviderScope: providerClaimScope(b.claimConfig("example-org/my-app")),
		Labels: map[string]string{
			labelRepository: "example-org/my-app",
			labelLogin:      "alice",
			labelUserID:     fmt.Sprintf("%d", user.ID),
		},
	}
	user.Login = "alice-renamed"
	if err := b.validateClaimScope(claim, user); err != nil {
		t.Fatal(err)
	}
}

func TestAcquireRetainsClaimWhenAmbiguousCreateHasNoMatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.createErr = errors.New("create response lost")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aa3"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "pending-box",
	})
	if err == nil || !strings.Contains(err.Error(), "claim retained") {
		t.Fatalf("err=%v", err)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted uncertain resource=%#v", fc.deletes)
	}
}

func TestAcquireRejectsDuplicateRecoveryMatches(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.createErr = errors.New("create response lost")
	fc.onCreate = func(req createCodespaceRequest) {
		for _, name := range []string{"cs-duplicate-one", "cs-duplicate-two"} {
			item := fakeCodespace(name, "Available")
			item.DisplayName = req.DisplayName
			item.Repository.FullName = req.Repo
			fc.items[name] = item
		}
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789aa4",
		RequestedSlug:    "duplicate-box",
	})
	if err == nil || !strings.Contains(err.Error(), "multiple github-codespaces resources match") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted ambiguous resources=%#v", fc.deletes)
	}
}

func TestAcquireDiscardsClaimAfterDefinitiveCreateRejection(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.createErr = githubAPIError(422, "", `{"message":"invalid machine"}`)
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aa5"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "rejected-box",
	})
	if err == nil || !strings.Contains(err.Error(), "status=422") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName); claimErr != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("delete after definitive rejection=%#v", fc.deletes)
	}
}

func TestAcquireRollsBackExactCreateWhenClaimBindingFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.bindClaim = func(_ string, expected LeaseClaim, _ Server) (LeaseClaim, error) {
		return expected, errors.New("claim write failed")
	}
	leaseID := "cbx_123456789aa9"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "bind-failure-box",
	})
	if err == nil || !strings.Contains(err.Error(), "claim write failed") {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(fc.deletes, ","); got != "cs-1" {
		t.Fatalf("deletes=%q", got)
	}
	if !fc.deleteDeadline {
		t.Fatal("rollback delete context had no deadline")
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName); claimErr != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
}

func TestAcquireDoesNotRollbackWhenPendingClaimRaces(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aaa"
	b.bindClaim = func(_ string, expected LeaseClaim, _ Server) (LeaseClaim, error) {
		server := serverFromClaim(expected)
		server.Labels["raced"] = "true"
		if err := updateLeaseClaimEndpoint(expected.LeaseID, server, SSHTarget{}); err != nil {
			t.Fatal(err)
		}
		return expected, errors.New("claim raced")
	}

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "race-box",
	})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted after claim race=%#v", fc.deletes)
	}
	claim, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName)
	if claimErr != nil || !ok || claim.Labels[labelRecovery] != recoveryPreCreate || claim.Labels["raced"] != "true" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestAcquireOnAcquiredErrorRollsBackEvenWhenKept(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789aaf"
	callbackErr := errors.New("controller rejected identity")
	called := false

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "callback-box",
		Keep:             true,
		OnAcquired: func(lease LeaseTarget) error {
			called = true
			if lease.LeaseID != leaseID || lease.Server.CloudID != "cs-1" || lease.SSH.Host != "" {
				t.Fatalf("callback lease=%#v", lease)
			}
			claim, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName)
			if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
				t.Fatalf("claim was bound before callback: claim=%#v ok=%t err=%v", claim, ok, claimErr)
			}
			return callbackErr
		},
	})
	if !called || !errors.Is(err, callbackErr) {
		t.Fatalf("called=%t err=%v", called, err)
	}
	if got := strings.Join(fc.deletes, ","); got != "cs-1" {
		t.Fatalf("deletes=%q", got)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
}

func TestAcquireOnAcquiredErrorRetainsPendingClaimWhenRollbackCannotConfirmResource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ab0"
	callbackErr := errors.New("controller rejected identity")

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "callback-missing-box",
		OnAcquired: func(lease LeaseTarget) error {
			delete(fc.items, lease.Server.CloudID)
			return callbackErr
		},
	})
	if !errors.Is(err, callbackErr) || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted unconfirmed resource=%#v", fc.deletes)
	}
	claim, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestAcquireOnAcquiredErrorRetainsClaimWhenResourceDisappearsAfterPreflight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	getCount := 0
	fc.onGet = func(name string) {
		getCount++
		if getCount == 2 {
			delete(fc.items, name)
		}
	}
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ab4"
	callbackErr := errors.New("controller rejected identity")

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "callback-vanished-box",
		OnAcquired: func(LeaseTarget) error {
			return callbackErr
		},
	})
	if !errors.Is(err, callbackErr) || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("err=%v", err)
	}
	if getCount != 2 || len(fc.deletes) != 0 {
		t.Fatalf("gets=%d deletes=%#v", getCount, fc.deletes)
	}
	claim, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "" || claim.Labels[labelRecovery] != recoveryPreCreate {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestRollbackCreatedCodespaceRetainsClaimWhenResourceDisappearsAfterPreflight(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-rollback-missing", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ab6"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "rollback-missing-box", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "rollback-missing-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !ok {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
	getCount := 0
	fc.onGet = func(name string) {
		getCount++
		if getCount == 2 {
			delete(fc.items, name)
		}
	}

	err = rollbackCreatedCodespace(fc, claim)
	if err == nil || !strings.Contains(err.Error(), "recovery claim retained") {
		t.Fatalf("err=%v", err)
	}
	if getCount != 2 || len(fc.deletes) != 0 {
		t.Fatalf("gets=%d deletes=%#v", getCount, fc.deletes)
	}
	retained, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || retained.CloudID != item.Name {
		t.Fatalf("claim=%#v ok=%t err=%v", retained, ok, claimErr)
	}
}

func TestAcquireOnAcquiredErrorRollsBackAfterDisplayNameChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ab5"
	callbackErr := errors.New("controller rejected identity")

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "callback-renamed-box",
		OnAcquired: func(lease LeaseTarget) error {
			item := fc.items[lease.Server.CloudID]
			item.DisplayName = "renamed-after-create"
			fc.items[item.Name] = item
			return callbackErr
		},
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(fc.deletes, ","); got != "cs-1" {
		t.Fatalf("deletes=%q", got)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
}

func TestAcquireRetainsClaimAndRefusesRollbackAfterReadyIdentityChanges(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	replacement := fakeCodespace("cs-1", "Available")
	replacement.EnvironmentID = "env-foreign-replacement"
	fc.getSeq["cs-1"] = []codespace{replacement}
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ab0"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "replacement-box",
	})
	if err == nil || !strings.Contains(err.Error(), "environment id changed") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("replacement deleted: %#v", fc.deletes)
	}
	claim, ok, claimErr := readLeaseClaimWithPresence(leaseID)
	if claimErr != nil || !ok || claim.CloudID != "cs-1" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, claimErr)
	}
}

func TestReleaseRecoversPendingCreateThenDeletesExactResource(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.createErr = errors.New("create response lost")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aa6"

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: leaseID,
		RequestedSlug:    "later-box",
	})
	if err == nil {
		t.Fatal("acquire unexpectedly succeeded")
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("pending claim ok=%t err=%v", ok, err)
	}
	item := fakeCodespace("cs-later", "Available")
	item.DisplayName = claim.Labels[labelDisplayName]
	item.Repository.FullName = "Example-Org/My-App"
	fc.items[item.Name] = item

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID}}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(fc.deletes, ","); got != item.Name {
		t.Fatalf("deletes=%q", got)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, err)
	}
}

func TestAcquireKeepDoesNotOverrideDeleteOnReleasePolicy(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{
		fakeCodespace("cs-1", "Provisioning"),
		fakeCodespace("cs-1", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	lease, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:          Repo{Root: t.TempDir(), Name: "my-app"},
		Keep:          true,
		RequestedSlug: "warm-box",
	})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Labels["keep"] != "true" || lease.Server.Labels[labelRelease] != releaseDelete {
		t.Fatalf("labels=%#v", lease.Server.Labels)
	}
	claim, ok, err := resolveLeaseClaimForProvider(lease.LeaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels["keep"] != "true" || claim.Labels[labelRelease] != releaseDelete {
		t.Fatalf("claim labels=%#v", claim.Labels)
	}
}

func TestAcquireRetainsClaimWhenRollbackDeleteFails(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.getSeq["cs-1"] = []codespace{fakeCodespace("cs-1", "Failed")}
	fc.deleteErr = errors.New("delete temporarily unavailable")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	_, err := b.Acquire(context.Background(), AcquireRequest{
		Repo:             Repo{Root: t.TempDir(), Name: "my-app"},
		RequestedLeaseID: "cbx_123456789abc",
		RequestedSlug:    "rollback-box",
	})
	if err == nil {
		t.Fatal("acquire unexpectedly succeeded")
	}
	for _, want := range []string{"terminal state=Failed", "rollback github-codespaces", "delete temporarily unavailable", "cs-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err=%q missing %q", err, want)
		}
	}
	if _, ok, err := resolveLeaseClaimForProvider("cbx_123456789abc", providerName); err != nil || !ok {
		t.Fatalf("recovery claim missing ok=%t err=%v", ok, err)
	}
	if !fc.deleteDeadline {
		t.Fatal("rollback delete context had no deadline")
	}
}

func TestResolveStartsStoppedCodespaceAndRefreshesTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopped"] = fakeCodespace("cs-stopped", "Shutdown")
	fc.getSeq["cs-stopped"] = []codespace{
		fakeCodespace("cs-stopped", "Shutdown"),
		fakeCodespace("cs-stopped", "Starting"),
		fakeCodespace("cs-stopped", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abc"
	server := b.serverFromCodespace(fc.items["cs-stopped"], b.labelsFor(leaseID, "sleepy-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopped"], "stopped"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "sleepy-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "sleepy-box", ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.starts) != 1 || fc.starts[0] != "cs-stopped" {
		t.Fatalf("starts=%#v", fc.starts)
	}
	if lease.Server.Status != "Available" || lease.SSH.Host != "cs.cs-stopped.main" {
		t.Fatalf("lease=%#v", lease)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.Labels[labelState] != "ready" || claim.SSHHost != "cs.cs-stopped.main" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestResolveWaitsForShutdownThenRestartsCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopping"] = fakeCodespace("cs-stopping", "ShuttingDown")
	fc.getSeq["cs-stopping"] = []codespace{
		fakeCodespace("cs-stopping", "ShuttingDown"),
		fakeCodespace("cs-stopping", "Shutdown"),
		fakeCodespace("cs-stopping", "Starting"),
		fakeCodespace("cs-stopping", "Available"),
	}
	startState := ""
	fc.onStart = func(name string) {
		startState = fc.items[name].State
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ad0"
	server := b.serverFromCodespace(fc.items["cs-stopping"], b.labelsFor(leaseID, "stopping-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopping"], "stopping"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stopping-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.starts) != 1 || fc.starts[0] != "cs-stopping" || startState != "Shutdown" || lease.Server.Status != "Available" || lease.SSH.Host != "cs.cs-stopping.main" {
		t.Fatalf("starts=%#v startState=%q lease=%#v", fc.starts, startState, lease)
	}
}

func TestResolveShuttingDownCodespaceUsesReadyTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopping"] = fakeCodespace("cs-stopping", "ShuttingDown")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.readyTimeout = time.Nanosecond
	b.pollInterval = time.Hour
	leaseID := "cbx_123456789ad1"
	server := b.serverFromCodespace(fc.items["cs-stopping"], b.labelsFor(leaseID, "stopping-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopping"], "stopping"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stopping-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	_, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReadyProbe: true})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveWaitsForTransitionalCodespaceBeforeSSH(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-starting"] = fakeCodespace("cs-starting", "Starting")
	fc.getSeq["cs-starting"] = []codespace{
		fakeCodespace("cs-starting", "Starting"),
		fakeCodespace("cs-starting", "Provisioning"),
		fakeCodespace("cs-starting", "Available"),
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ae1"
	server := b.serverFromCodespace(fc.items["cs-starting"], b.labelsFor(leaseID, "starting-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-starting"], "provisioning"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "starting-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "Available" || lease.SSH.Host != "cs.cs-starting.main" || len(fc.starts) != 0 || fg.configFor != "cs-starting" {
		t.Fatalf("lease=%#v starts=%#v config=%q", lease, fc.starts, fg.configFor)
	}
}

func TestResolveStatusOnlyReadyProbeDoesNotStartStoppedCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopped-status"] = fakeCodespace("cs-stopped-status", "Shutdown")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac5"
	server := b.serverFromCodespace(fc.items["cs-stopped-status"], b.labelsFor(leaseID, "stopped-status-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopped-status"], "stopped"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stopped-status-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "stopped-status-box", StatusOnly: true, ReadyProbe: true, NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.starts) != 0 || lease.SSH.Host != "" || len(b.waits) != 0 {
		t.Fatalf("starts=%#v lease=%#v waits=%#v", fc.starts, lease, b.waits)
	}
	if lease.Server.Status != "Shutdown" || lease.Server.Labels[labelState] != "stopped" {
		t.Fatalf("lease=%#v", lease)
	}
}

func TestResolveStatusOnlyNormalizesStoppedCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stopped-status"] = fakeCodespace("cs-stopped-status", "Shutdown")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac6"
	server := b.serverFromCodespace(fc.items["cs-stopped-status"], b.labelsFor(leaseID, "stopped-status-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stopped-status"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stopped-status-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, StatusOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Server.Status != "Shutdown" || lease.Server.Labels[labelState] != "stopped" || len(fc.starts) != 0 || len(b.waits) != 0 {
		t.Fatalf("lease=%#v starts=%#v waits=%#v", lease, fc.starts, b.waits)
	}
}

func TestResolveNoLocalStateMutationsDoesNotStartStoppedCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-readonly-stopped"] = fakeCodespace("cs-readonly-stopped", "Shutdown")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac7"
	server := b.serverFromCodespace(fc.items["cs-readonly-stopped"], b.labelsFor(leaseID, "readonly-stopped", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-readonly-stopped"], "stopped"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "readonly-stopped", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: leaseID, NoLocalStateMutations: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.starts) != 0 || lease.SSH.Host != "" || fg.configFor != "" || lease.Server.Labels[labelState] != "stopped" {
		t.Fatalf("starts=%#v lease=%#v config=%q", fc.starts, lease, fg.configFor)
	}
}

func TestResolveNoLocalStateMutationsDoesNotStoreSSHConfig(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fc.items["cs-readonly"] = fakeCodespace("cs-readonly", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac3"
	server := b.serverFromCodespace(fc.items["cs-readonly"], b.labelsFor(leaseID, "readonly-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-readonly"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "readonly-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "readonly-box", NoLocalStateMutations: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "cs.cs-readonly.main" {
		t.Fatalf("lease=%#v", lease)
	}
	stored := filepath.Join(stateHome, "crabbox", "github-codespaces", leaseID+".ssh_config")
	if _, err := os.Stat(stored); !os.IsNotExist(err) {
		t.Fatalf("stored config err=%v path=%s", err, stored)
	}
}

func TestResolveStatusOnlyReadyProbeBuildsSSHTarget(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-status"] = fakeCodespace("cs-status", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac4"
	server := b.serverFromCodespace(fc.items["cs-status"], b.labelsFor(leaseID, "status-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-status"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "status-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	lease, err := b.Resolve(context.Background(), ResolveRequest{ID: "status-box", StatusOnly: true, ReadyProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	if lease.SSH.Host != "cs.cs-status.main" || len(b.waits) != 1 {
		t.Fatalf("lease=%#v waits=%#v", lease, b.waits)
	}
}

func TestReleaseDeleteRemovesOnlyClaimBackedCodespaceAndConfig(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-delete"] = fakeCodespace("cs-delete", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abd"
	server := b.serverFromCodespace(fc.items["cs-delete"], b.labelsFor(leaseID, "delete-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-delete"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "delete-box", b.cfg, server, SSHTarget{Host: "cs-delete", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	configPath, err := storeSSHConfig(leaseID, fg.config("cs-delete"))
	if err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.deletes, ",") != "cs-delete" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim remains ok=%t err=%v", ok, err)
	}
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("stored config remains err=%v path=%s", err, configPath)
	}
}

func TestReleaseDeleteRetainsClaimUntilAbsenceIsConfirmed(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-delete-pending"] = fakeCodespace("cs-delete-pending", "Available")
	fc.deleteKeepsItem = true
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ab1"
	server := b.serverFromCodespace(fc.items["cs-delete-pending"], b.labelsFor(leaseID, "delete-pending-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-delete-pending"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "delete-pending-box", b.cfg, server, SSHTarget{Host: "cs-delete-pending", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	configPath, err := storeSSHConfig(leaseID, fg.config("cs-delete-pending"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fc.onDelete = func(string) { cancel() }
	err = b.ReleaseLease(ctx, ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
	if strings.Join(fc.deletes, ",") != "cs-delete-pending" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
	if _, ok, claimErr := resolveLeaseClaimForProvider(leaseID, providerName); claimErr != nil || !ok {
		t.Fatalf("claim retained ok=%t err=%v", ok, claimErr)
	}
	if _, statErr := os.Stat(configPath); statErr != nil {
		t.Fatalf("ssh config removed before confirmed absence: %v", statErr)
	}
}

func TestReleaseDeleteRejectsClaimRaceBeforeMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-race-delete"] = fakeCodespace("cs-race-delete", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aab"
	server := b.serverFromCodespace(fc.items["cs-race-delete"], b.labelsFor(leaseID, "race-delete-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-race-delete"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "race-delete-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	fc.onGet = func(string) {
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil || !ok {
			t.Fatalf("claim ok=%t err=%v", ok, err)
		}
		raced := serverFromClaim(claim)
		raced.Labels["raced"] = "true"
		if err := updateLeaseClaimEndpoint(leaseID, raced, SSHTarget{}); err != nil {
			t.Fatal(err)
		}
	}

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted after claim race=%#v", fc.deletes)
	}
}

func TestReleaseDeleteRequiresLocalClaim(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-orphan"] = fakeCodespace("cs-orphan", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	server := b.serverFromCodespace(fc.items["cs-orphan"], b.labelsFor("cbx_123456789ad0", "orphan-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-orphan"], "ready"))

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: "cbx_123456789ad0", Server: server}})
	if err == nil || !strings.Contains(err.Error(), "requires a local claim") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 || len(fc.stops) != 0 {
		t.Fatalf("provider action without claim deletes=%#v stops=%#v", fc.deletes, fc.stops)
	}
}

func TestReleaseRefusesProviderScopeMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-scope"] = fakeCodespace("cs-scope", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aa7"
	server := b.serverFromCodespace(fc.items["cs-scope"], b.labelsFor(leaseID, "scope-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-scope"], "ready"))
	server.Labels[labelDisplayName] = "crabbox-scope-box"
	if err := claimLeaseTargetForRepoConfig(leaseID, "scope-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	b.cfg.GitHubCodespaces.APIURL = "https://api.enterprise.example"

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "scope mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 || len(fc.stops) != 0 {
		t.Fatalf("mutated on scope mismatch deletes=%#v stops=%#v", fc.deletes, fc.stops)
	}
}

func TestReleaseDeleteFallsBackToStopForDirtyCodespace(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-dirty", "Available")
	item.GitStatus.HasUncommittedChanges = true
	fc.items["cs-dirty"] = item
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac2"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "dirty-box", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "dirty-box", b.cfg, server, SSHTarget{Host: "cs-dirty", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.stops, ",") != "cs-dirty" || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelRelease] != releaseStop || claim.Labels[labelState] != "stopped" {
		t.Fatalf("claim=%#v", claim)
	}
	if !b.RetainLeaseClaimAfterRelease(LeaseTarget{LeaseID: leaseID, Server: server}) {
		t.Fatal("dirty release fallback should retain local claim")
	}
	if got := b.ReleaseLeaseMessage(LeaseTarget{LeaseID: leaseID, Server: server}); !strings.Contains(got, "retained=true") {
		t.Fatalf("message=%q", got)
	}
}

func TestReleaseDirtyCodespaceRefusesZeroRetentionWithoutStopping(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-zero-retention", "Available")
	item.GitStatus.HasUncommittedChanges = true
	zero := 0
	item.RetentionPeriodMinutes = &zero
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "test" + "-value"})
	leaseID := "cbx_123456789af3"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "zero-retention", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "zero-retention", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "effective retention is zero") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
}

func TestReleaseCleanCodespaceRefusesZeroRetentionWithoutStopping(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-clean-zero-retention", "Available")
	zero := 0
	item.RetentionPeriodMinutes = &zero
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "test" + "-value"})
	leaseID := "cbx_123456789af6"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "clean-zero-retention", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "clean-zero-retention", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "effective retention is zero") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
}

func TestCleanupDryRunReportsDirtyRetentionWithoutMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-dirty-dry-run", "Available")
	item.GitStatus.HasUncommittedChanges = true
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "test" + "-value"})
	var stderr bytes.Buffer
	b.rt.Stderr = &stderr
	leaseID := "cbx_123456789af4"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "dirty-dry-run", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "dirty-dry-run", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "retain codespace=cs-dirty-dry-run") || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("stderr=%q stops=%#v deletes=%#v", stderr.String(), fc.stops, fc.deletes)
	}
}

func TestCodespaceTerminalIncludesArchivedAndMoved(t *testing.T) {
	for _, state := range []string{"Archived", "Moved"} {
		if !codespaceTerminal(state) {
			t.Fatalf("state %q was not terminal", state)
		}
	}
}

func TestReleaseDeleteRechecksGitStatusAfterStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-dirty-race"] = fakeCodespace("cs-dirty-race", "Available")
	fc.onStop = func(name string) {
		item := fc.items[name]
		item.GitStatus.HasUnpushedChanges = true
		fc.items[name] = item
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac5"
	server := b.serverFromCodespace(fc.items["cs-dirty-race"], b.labelsFor(leaseID, "dirty-race-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-dirty-race"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "dirty-race-box", b.cfg, server, SSHTarget{Host: "cs-dirty-race", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deletes) != 0 || len(fc.stops) != 2 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok || claim.Labels[labelRelease] != releaseStop || claim.Labels[labelState] != "stopped" {
		t.Fatalf("claim=%#v ok=%t err=%v", claim, ok, err)
	}
}

func TestReleaseDeleteRechecksIdentityAfterStop(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-identity-race"] = fakeCodespace("cs-identity-race", "Available")
	fc.onStop = func(name string) {
		item := fc.items[name]
		item.EnvironmentID = "different-environment"
		fc.items[name] = item
	}
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac6"
	server := b.serverFromCodespace(fc.items["cs-identity-race"], b.labelsFor(leaseID, "identity-race-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-identity-race"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "identity-race-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}})
	if err == nil || !strings.Contains(err.Error(), "environment id changed") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted after identity race=%#v", fc.deletes)
	}
}

func TestReleaseRetainedStopsAndClearsEndpoint(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-stop"] = fakeCodespace("cs-stop", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.cfg.GitHubCodespaces.DeleteOnRelease = false
	leaseID := "cbx_123456789abe"
	server := b.serverFromCodespace(fc.items["cs-stop"], b.labelsFor(leaseID, "stop-box", "example-org/my-app", "alice", true, releaseStop, fc.items["cs-stop"], "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "stop-box", b.cfg, server, SSHTarget{Host: "cs-stop", Port: "22"}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.stops, ",") != "cs-stop" || len(fc.deletes) != 0 {
		t.Fatalf("stops=%#v deletes=%#v", fc.stops, fc.deletes)
	}
	claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
	if err != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	if claim.SSHHost != "" || claim.SSHPort != 0 || claim.Labels[labelRelease] != releaseStop || claim.Labels[labelState] != "stopped" {
		t.Fatalf("claim=%#v", claim)
	}
}

func TestReleaseRetainedRemovesClaimWhenCodespaceIsAlreadyAbsent(t *testing.T) {
	stateHome := t.TempDir()
	t.Setenv("XDG_STATE_HOME", stateHome)
	fc := newFakeCodespacesClient()
	fc.stopErr = githubAPIError(404, "", `{"message":"Not Found"}`)
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.cfg.GitHubCodespaces.DeleteOnRelease = false
	leaseID := "cbx_123456789ab9"
	item := fakeCodespace("cs-retained-absent", "Shutdown")
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "retained-absent-box", "example-org/my-app", "alice", true, releaseStop, item, "stopped"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "retained-absent-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	if _, err := storeSSHConfig(leaseID, "Host retained-absent-box\n"); err != nil {
		t.Fatal(err)
	}

	if err := b.ReleaseLease(context.Background(), ReleaseLeaseRequest{Lease: LeaseTarget{LeaseID: leaseID, Server: server}}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := resolveLeaseClaimForProvider(leaseID, providerName); err != nil || ok {
		t.Fatalf("claim ok=%t err=%v", ok, err)
	}
	configPath := filepath.Join(stateHome, "crabbox", "github-codespaces", leaseID+".ssh_config")
	if _, err := os.Stat(configPath); !os.IsNotExist(err) {
		t.Fatalf("stored config err=%v path=%s", err, configPath)
	}
}

func TestCleanupDryRunKeepsProviderNonMutating(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-expired"] = fakeCodespace("cs-expired", "Available")
	fc.items["cs-unclaimed"] = fakeCodespace("cs-unclaimed", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789abf"
	server := b.serverFromCodespace(fc.items["cs-expired"], b.labelsFor(leaseID, "expired-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-expired"], "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "expired-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("dry run deleted: %#v", fc.deletes)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.deletes, ",") != "cs-expired" {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
}

func TestCleanupRecoversExpiredPendingCreateBeforeDelete(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ae2"
	now := time.Now().UTC()
	b.now = func() time.Time { return now.Add(-14 * time.Hour) }
	claim, displayName := persistPendingRecoveryClaimForTest(t, b, leaseID, "pending-cleanup", "pending-cleanup-nonce", true)
	b.now = func() time.Time { return now }
	item := fakeCodespace("cs-pending-cleanup", "Available")
	item.DisplayName = displayName
	fc.items[item.Name] = item

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	claim, ok, err := readLeaseClaimWithPresence(leaseID)
	if err != nil || !ok || claim.CloudID != "" || len(fc.deletes) != 0 {
		t.Fatalf("dry-run claim=%#v ok=%t err=%v deletes=%#v", claim, ok, err, fc.deletes)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(fc.deletes, ",") != item.Name {
		t.Fatalf("deletes=%#v", fc.deletes)
	}
	if _, ok, err := readLeaseClaimWithPresence(leaseID); err != nil || ok {
		t.Fatalf("claim remained ok=%t err=%v", ok, err)
	}
}

func TestCleanupDiscardsExpiredPendingCreateWithoutInventoryMatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ae3"
	now := time.Now().UTC()
	b.now = func() time.Time { return now.Add(-14 * time.Hour) }
	persistPendingRecoveryClaimForTest(t, b, leaseID, "pending-absent", "pending-absent-nonce", true)
	b.now = func() time.Time { return now }

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readLeaseClaimWithPresence(leaseID); err != nil || !ok {
		t.Fatalf("dry-run claim ok=%t err=%v", ok, err)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readLeaseClaimWithPresence(leaseID); err != nil || ok {
		t.Fatalf("claim remained ok=%t err=%v", ok, err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted unconfirmed resource=%#v", fc.deletes)
	}
}

func TestCleanupDiscardsExpiredBoundClaimAfterConfirmedAbsence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ae4"
	now := time.Now().UTC()
	b.now = func() time.Time { return now.Add(-14 * time.Hour) }
	item := fakeCodespace("cs-bound-absent", "Available")
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "bound-absent", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "bound-absent", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return now }

	if err := b.Cleanup(context.Background(), CleanupRequest{DryRun: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readLeaseClaimWithPresence(leaseID); err != nil || !ok {
		t.Fatalf("dry-run claim ok=%t err=%v", ok, err)
	}
	if err := b.Cleanup(context.Background(), CleanupRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := readLeaseClaimWithPresence(leaseID); err != nil || ok {
		t.Fatalf("claim remained ok=%t err=%v", ok, err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted already-absent resource=%#v", fc.deletes)
	}
}

func TestCleanupRetainsExpiredAbsentClaimForDifferentGitHubIdentity(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ae5"
	now := time.Now().UTC()
	b.now = func() time.Time { return now.Add(-14 * time.Hour) }
	item := fakeCodespace("cs-other-account", "Available")
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "other-account", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "other-account", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	b.now = func() time.Time { return now }
	fc.user = fakeGitHubUser("bob")

	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("err=%v", err)
	}
	if _, ok, claimErr := readLeaseClaimWithPresence(leaseID); claimErr != nil || !ok {
		t.Fatalf("claim ok=%t err=%v", ok, claimErr)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted other account resource=%#v", fc.deletes)
	}
}

func TestCleanupRefusesIdentityMismatch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-mismatch"] = fakeCodespace("cs-mismatch", "Available")
	fg := &fakeGH{login: "bob", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789ac1"
	server := b.serverFromCodespace(fc.items["cs-mismatch"], b.labelsFor(leaseID, "mismatch-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-mismatch"], "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "mismatch-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted on mismatch: %#v", fc.deletes)
	}
}

func TestCleanupRejectsClaimRaceBeforeMutation(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-cleanup-race"] = fakeCodespace("cs-cleanup-race", "Available")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	leaseID := "cbx_123456789aac"
	server := b.serverFromCodespace(fc.items["cs-cleanup-race"], b.labelsFor(leaseID, "cleanup-race-box", "example-org/my-app", "alice", false, releaseDelete, fc.items["cs-cleanup-race"], "ready"))
	server.Labels["expires_at"] = time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)
	if err := claimLeaseTargetForRepoConfig(leaseID, "cleanup-race-box", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	fc.onGet = func(string) {
		claim, ok, err := resolveLeaseClaimForProvider(leaseID, providerName)
		if err != nil || !ok {
			t.Fatalf("claim ok=%t err=%v", ok, err)
		}
		raced := serverFromClaim(claim)
		raced.Labels["raced"] = "true"
		if err := updateLeaseClaimEndpoint(leaseID, raced, SSHTarget{}); err != nil {
			t.Fatal(err)
		}
	}

	err := b.Cleanup(context.Background(), CleanupRequest{})
	if err == nil || !strings.Contains(err.Error(), "claim changed") {
		t.Fatalf("err=%v", err)
	}
	if len(fc.deletes) != 0 {
		t.Fatalf("deleted after cleanup claim race=%#v", fc.deletes)
	}
}

func TestListAllowsUserRenamedDisplayName(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-renamed-display", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ad1"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "renamed-display", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	server.Labels[labelDisplayName] = "original-display"
	if err := claimLeaseTargetForRepoConfig(leaseID, "renamed-display", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}
	item.DisplayName = "user-renamed-display"
	fc.items[item.Name] = item

	views, err := b.List(context.Background(), ListRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 || views[0].CloudID != item.Name {
		t.Fatalf("views=%#v", views)
	}
}

func TestDoctorIsNonMutating(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.Message, "mutation=false") || !strings.Contains(result.Message, "inventory=ready") {
		t.Fatalf("result=%#v", result)
	}
	if len(fc.creates) != 0 || len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("doctor mutated: creates=%#v starts=%#v stops=%#v deletes=%#v", fc.creates, fc.starts, fc.stops, fc.deletes)
	}
}

func TestDoctorFailsClosedAfterGitHubAccountSwitch(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	item := fakeCodespace("cs-other-account", "Available")
	fc.items[item.Name] = item
	b := newTestBackend(t, fc, &fakeGH{login: "bob", token: "ghp_this_token_value_is_redacted"})
	leaseID := "cbx_123456789ad2"
	server := b.serverFromCodespace(item, b.labelsFor(leaseID, "other-account", "example-org/my-app", "alice", false, releaseDelete, item, "ready"))
	if err := claimLeaseTargetForRepoConfig(leaseID, "other-account", b.cfg, server, SSHTarget{}, t.TempDir(), time.Hour, false); err != nil {
		t.Fatal(err)
	}

	result, err := b.Doctor(context.Background(), DoctorRequest{})
	if err == nil || result.Status != "failed" || !strings.Contains(err.Error(), "account mismatch") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(fc.starts) != 0 || len(fc.stops) != 0 || len(fc.deletes) != 0 {
		t.Fatalf("doctor mutated: starts=%#v stops=%#v deletes=%#v", fc.starts, fc.stops, fc.deletes)
	}
}

func TestControlPlanePrefersGitHubCLITokenPrecedence(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "gh-token")
	t.Setenv("GITHUB_TOKEN", "github-token")
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "fallback-token"}
	b := newTestBackend(t, fc, fg)
	var gotToken string
	b.clientFactory = func(token string) codespacesAPI {
		gotToken = token
		return fc
	}

	_, _, user, err := b.controlPlane(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if user.Login != "alice" || user.ID != fakeGitHubUser("alice").ID {
		t.Fatalf("user=%#v", user)
	}
	if gotToken != "gh-token" {
		t.Fatalf("token=%q", gotToken)
	}
}

func TestControlPlaneUsesEnterpriseTokenForCustomAPIHost(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("GH_TOKEN", "dotcom-token")
	t.Setenv("GITHUB_TOKEN", "dotcom-fallback-token")
	t.Setenv("GH_ENTERPRISE_TOKEN", "enterprise-token")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "enterprise-fallback-token")
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "stored-token"}
	b := newTestBackend(t, fc, fg)
	b.cfg.GitHubCodespaces.APIURL = "https://github.enterprise.example/api/v3"
	var gotToken string
	b.clientFactory = func(token string) codespacesAPI {
		gotToken = token
		return fc
	}

	if _, _, _, err := b.controlPlane(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotToken != "enterprise-token" {
		t.Fatalf("selected wrong token family")
	}
}

func TestWaitForAvailableUsesReadyTimeout(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	fc := newFakeCodespacesClient()
	fc.items["cs-slow"] = fakeCodespace("cs-slow", "Provisioning")
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)
	b.readyTimeout = time.Nanosecond
	b.pollInterval = time.Hour

	_, err := b.waitForAvailable(context.Background(), fc, "cs-slow")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
}

func TestEffectiveWorkRootHonorsExplicitGenericWorkRoot(t *testing.T) {
	cfg := Config{
		Provider: providerName,
		WorkRoot: "/custom/workspace",
		GitHubCodespaces: GitHubCodespacesConfig{
			WorkRoot: defaultWorkRoot,
		},
	}
	core.MarkWorkRootExplicit(&cfg)
	b := newBackend(Provider{}.Spec(), cfg, Runtime{})

	if got := b.effectiveWorkRoot("example-org/my-app"); got != "/custom/workspace" {
		t.Fatalf("work root=%q", got)
	}
}

func TestRepoConfigReadyCheckUsesEffectiveWorkRoot(t *testing.T) {
	b := newBackend(Provider{}.Spec(), Config{GitHubCodespaces: GitHubCodespacesConfig{WorkRoot: defaultWorkRoot}}, Runtime{})
	check := githubCodespacesReadyCheck(b.repoConfig("example-org/my-app"))
	if !strings.Contains(check, "'/workspaces/my-app'") || strings.Contains(check, "'/workspaces/crabbox'") {
		t.Fatalf("ready check=%q", check)
	}
}

func TestLabelsCarryEffectiveWorkRoot(t *testing.T) {
	fc := newFakeCodespacesClient()
	fg := &fakeGH{login: "alice", token: "ghp_this_token_value_is_redacted"}
	b := newTestBackend(t, fc, fg)

	labels := b.labelsFor("cbx_123456789abc", "work-box", "example-org/my-app", "alice", false, releaseDelete, fakeCodespace("cs-1", "Available"), "ready")
	if labels["work_root"] != "/workspaces/my-app" {
		t.Fatalf("work_root=%q", labels["work_root"])
	}
}

func TestDisplayNameFitsGitHubCodespacesLimit(t *testing.T) {
	name := githubCodespacesDisplayName("cbx_abcdef123456", strings.Repeat("a", 41), "nonce-a")
	if len(name) > 48 {
		t.Fatalf("display name length=%d name=%q", len(name), name)
	}
	if !strings.HasPrefix(name, "crabbox-") || !strings.Contains(name, "-c80c2195-") {
		t.Fatalf("display name=%q", name)
	}
	if other := githubCodespacesDisplayName("cbx_abcdef123456", strings.Repeat("a", 41), "nonce-b"); other == name {
		t.Fatalf("recovery nonce did not affect display name: %q", name)
	}
}

type testBackend struct {
	*backend
	waits []SSHTarget
}

func newTestBackend(t *testing.T, fc *fakeCodespacesClient, fg *fakeGH) *testBackend {
	t.Helper()
	cfg := Config{
		Provider:    providerName,
		TargetOS:    targetLinux,
		SSHUser:     "vscode",
		SSHPort:     "22",
		IdleTimeout: time.Hour,
		GitHubCodespaces: GitHubCodespacesConfig{
			APIURL:           defaultAPIURL,
			GHPath:           "gh",
			Repo:             "example-org/my-app",
			Ref:              "main",
			Machine:          "standardLinux32gb",
			DevcontainerPath: ".devcontainer/devcontainer.json",
			WorkingDirectory: "/workspaces/my-app",
			Geo:              "UsWest",
			IdleTimeout:      45 * time.Minute,
			RetentionPeriod:  48 * time.Hour,
			DeleteOnRelease:  true,
			WorkRoot:         defaultWorkRoot,
		},
	}
	rt := Runtime{}
	b := newBackend(Provider{}.Spec(), cfg, rt)
	fc.user = fakeGitHubUser(fg.login)
	b.pollInterval = time.Nanosecond
	tb := &testBackend{backend: b}
	b.clientFactory = func(string) codespacesAPI { return fc }
	b.ghFactory = func() githubCLI { return fg }
	b.waitSSH = func(_ context.Context, target *SSHTarget, _ string, _ time.Duration) error {
		tb.waits = append(tb.waits, *target)
		return nil
	}
	return tb
}

type fakeCodespacesClient struct {
	user            githubUser
	items           map[string]codespace
	getSeq          map[string][]codespace
	creates         []createCodespaceRequest
	createErr       error
	createResult    codespace
	useCreateResult bool
	onCreate        func(createCodespaceRequest)
	onGet           func(string)
	onStart         func(string)
	onStop          func(string)
	listErr         error
	starts          []string
	stops           []string
	stopErr         error
	deletes         []string
	deleteErr       error
	deleteDeadline  bool
	deleteKeepsItem bool
	onDelete        func(string)
}

func (f *fakeCodespacesClient) currentUser(context.Context) (githubUser, error) {
	if f.user.ID <= 0 || strings.TrimSpace(f.user.Login) == "" {
		return githubUser{}, errors.New("fake authenticated user is not configured")
	}
	return f.user, nil
}

func newFakeCodespacesClient() *fakeCodespacesClient {
	return &fakeCodespacesClient{
		items:  map[string]codespace{},
		getSeq: map[string][]codespace{},
	}
}

func (f *fakeCodespacesClient) createCodespace(_ context.Context, req createCodespaceRequest) (codespace, error) {
	f.creates = append(f.creates, req)
	if f.onCreate != nil {
		f.onCreate(req)
	}
	if f.createErr != nil {
		return codespace{}, f.createErr
	}
	item := f.createResult
	if !f.useCreateResult {
		item = fakeCodespace(fmt.Sprintf("cs-%d", len(f.creates)), "Provisioning")
	}
	item.DisplayName = req.DisplayName
	item.Repository.FullName = req.Repo
	item.Machine.Name = req.Machine
	if item.Name != "" {
		f.items[item.Name] = item
	}
	return item, nil
}

func (f *fakeCodespacesClient) listCodespaces(context.Context) ([]codespace, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]codespace, 0, len(f.items))
	for _, item := range f.items {
		out = append(out, item)
	}
	return out, nil
}

func (f *fakeCodespacesClient) getCodespace(_ context.Context, name string) (codespace, error) {
	if f.onGet != nil {
		f.onGet(name)
	}
	if seq := f.getSeq[name]; len(seq) > 0 {
		item := seq[0]
		f.getSeq[name] = seq[1:]
		f.items[name] = item
		return item, nil
	}
	item, ok := f.items[name]
	if !ok {
		return codespace{}, githubAPIError(404, "", `{"message":"Not Found"}`)
	}
	return item, nil
}

func (f *fakeCodespacesClient) startCodespace(_ context.Context, name string) (codespace, error) {
	if f.onStart != nil {
		f.onStart(name)
	}
	f.starts = append(f.starts, name)
	item := f.items[name]
	item.State = "Starting"
	f.items[name] = item
	return item, nil
}

func (f *fakeCodespacesClient) stopCodespace(_ context.Context, name string) error {
	f.stops = append(f.stops, name)
	if f.stopErr != nil {
		return f.stopErr
	}
	item := f.items[name]
	item.State = "Shutdown"
	f.items[name] = item
	if f.onStop != nil {
		f.onStop(name)
	}
	return nil
}

func (f *fakeCodespacesClient) deleteCodespace(ctx context.Context, name string) error {
	_, f.deleteDeadline = ctx.Deadline()
	f.deletes = append(f.deletes, name)
	if f.deleteErr != nil {
		return f.deleteErr
	}
	if f.onDelete != nil {
		f.onDelete(name)
	}
	if !f.deleteKeepsItem {
		delete(f.items, name)
	}
	return nil
}

func (f *fakeCodespacesClient) listMachines(context.Context, string, string) ([]codespaceMachine, error) {
	return []codespaceMachine{{Name: "standardLinux32gb"}}, nil
}

type fakeGH struct {
	login     string
	token     string
	configFor string
}

func (f *fakeGH) authStatus(context.Context) error { return nil }
func (f *fakeGH) authToken(context.Context) (string, error) {
	return f.token, nil
}
func (f *fakeGH) codespaceSSHConfig(_ context.Context, codespace string) (string, error) {
	f.configFor = codespace
	return f.config(codespace), nil
}
func (f *fakeGH) config(codespace string) string {
	return fmt.Sprintf(`Host cs.%s.main
  User vscode
  IdentityFile "/tmp/codespaces/key"
  UserKnownHostsFile /dev/null
  ProxyCommand gh codespace ssh -c %s --stdio
`, codespace, codespace)
}

func fakeCodespace(name, state string) codespace {
	retentionMinutes := 7 * 24 * 60
	return codespace{
		ID:                     int64(len(name) + 100),
		Name:                   name,
		DisplayName:            "Crabbox",
		State:                  state,
		EnvironmentID:          "env-" + name,
		RetentionPeriodMinutes: &retentionMinutes,
		Owner:                  fakeGitHubUser("alice"),
		Repository:             repositoryRef{ID: 1001, FullName: "example-org/my-app"},
		Machine:                machineRef{Name: "standardLinux32gb"},
		GitStatus: gitStatus{
			aheadPresent:       true,
			unpushedPresent:    true,
			uncommittedPresent: true,
		},
	}
}

func fakeGitHubUser(login string) githubUser {
	login = strings.TrimSpace(login)
	id := int64(42)
	if !strings.EqualFold(login, "alice") {
		id = 43
	}
	return githubUser{ID: id, Login: login}
}
