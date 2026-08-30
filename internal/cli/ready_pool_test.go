package cli

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type readyPoolWorkspaceOwnerTransport struct {
	events        *[]string
	releaseErr    error
	beforeRelease func()
}

func (t readyPoolWorkspaceOwnerTransport) Do(_ context.Context, req workspaceOwnerRemoteRequest) (string, error) {
	*t.events = append(*t.events, "owner."+string(req.Action))
	if t.beforeRelease != nil {
		t.beforeRelease()
	}
	if t.releaseErr != nil {
		return "", t.releaseErr
	}
	return "RELEASED", nil
}

func TestReadyPoolReturnNeedsHydrationStop(t *testing.T) {
	for _, tc := range []struct {
		result string
		want   bool
	}{
		{result: "ready", want: false},
		{result: "drain", want: true},
		{result: "release", want: true},
		{result: "", want: false},
	} {
		if got := readyPoolReturnNeedsHydrationStop(tc.result); got != tc.want {
			t.Fatalf("readyPoolReturnNeedsHydrationStop(%q)=%v, want %v", tc.result, got, tc.want)
		}
	}
}

func TestReadyPoolReturnReleasesWorkspaceOwnerBeforeCoordinator(t *testing.T) {
	for _, disposition := range []string{"ready", "drain", "release"} {
		t.Run(disposition, func(t *testing.T) {
			events := []string{}
			done := make(chan struct{})
			close(done)
			owner := &workspaceOwner{
				transport: readyPoolWorkspaceOwnerTransport{events: &events},
				key:       strings.Repeat("a", 64),
				token:     strings.Repeat("b", 64),
				stop:      make(chan struct{}),
				done:      done,
			}
			err := returnReadyPoolAfterWorkspaceOwner(context.Background(), &owner, func(context.Context) error {
				events = append(events, "coordinator."+disposition)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			// The run defer stops the borrow heartbeat only after this helper returns.
			events = append(events, "heartbeat.stop")
			want := "owner.release,coordinator." + disposition + ",heartbeat.stop"
			if got := strings.Join(events, ","); got != want {
				t.Fatalf("event order=%q, want %q", got, want)
			}
			if owner != nil {
				t.Fatal("released owner remained installed for the outer defer")
			}
		})
	}
}

func TestReadyPoolReturnStopsWhenWorkspaceOwnerReleaseFails(t *testing.T) {
	events := []string{}
	done := make(chan struct{})
	close(done)
	owner := &workspaceOwner{
		transport: readyPoolWorkspaceOwnerTransport{events: &events, releaseErr: errors.New("release response lost")},
		key:       strings.Repeat("a", 64),
		token:     strings.Repeat("b", 64),
		stop:      make(chan struct{}),
		done:      done,
	}
	returned := false
	err := returnReadyPoolAfterWorkspaceOwner(context.Background(), &owner, func(context.Context) error {
		returned = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "ambiguous remote state") {
		t.Fatalf("release error=%v, want ambiguous failure", err)
	}
	if returned {
		t.Fatal("coordinator return ran after workspace owner release failure")
	}
	if got := strings.Join(events, ","); got != "owner.release" {
		t.Fatalf("events=%q, want only owner release", got)
	}
	if owner != nil {
		t.Fatal("failed-close owner remained installed for a double close")
	}
}

func TestReadyPoolReturnGetsFreshCoordinatorBudgetAfterOwnerRelease(t *testing.T) {
	events := []string{}
	started, allow, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	close(done)
	owner := &workspaceOwner{
		transport: readyPoolWorkspaceOwnerTransport{events: &events, beforeRelease: func() { close(started); <-allow }},
		stop:      make(chan struct{}), done: done,
	}
	result := make(chan error, 1)
	go func() {
		result <- returnReadyPoolAfterWorkspaceOwner(context.Background(), &owner, func(ctx context.Context) error {
			events = append(events, "coordinator.return")
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) <= 29*time.Second || time.Until(deadline) > 30*time.Second {
				return errors.New("coordinator return did not receive a fresh 30s budget")
			}
			return nil
		})
	}()
	<-started
	close(allow)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(events, ","); got != "owner.release,coordinator.return" {
		t.Fatalf("event order=%q", got)
	}
}

func TestReadyPoolBorrowInputIncludesCompatibilityKey(t *testing.T) {
	input := readyPoolBorrowInput("example-org/my-app", "main", "abc123", "setup-v2", "linux-16-vcpu", "", "linux")
	if got := readyPoolInputString(input, "compatibilityKey"); got != "linux-16-vcpu" {
		t.Fatalf("compatibility key=%q", got)
	}
	if _, ok := input["provider"]; ok {
		t.Fatalf("empty provider unexpectedly constrained compatible pool: %#v", input)
	}
}

func TestReadyPoolRunBorrowInputForRunRequiresExactNoSyncCommit(t *testing.T) {
	repo := Repo{Head: "aaa", BaseRef: "main"}
	input, err := readyPoolRunBorrowInputForRun(Config{}, repo, "openclaw/openclaw", true)
	if err != nil {
		t.Fatalf("no-sync exact head input failed: %v", err)
	}
	if _, ok := input["allowMissingCommit"]; ok {
		t.Fatalf("no-sync exact head input allowed missing commit: %#v", input)
	}
	if heartbeat, _ := input["heartbeat"].(bool); !heartbeat {
		t.Fatalf("run borrow input did not negotiate heartbeat support: %#v", input)
	}

	_, err = readyPoolRunBorrowInputForRun(Config{Actions: ActionsConfig{Ref: "feature"}}, Repo{BaseRef: "main"}, "openclaw/openclaw", true)
	if err == nil {
		t.Fatal("no-sync ref-only input succeeded")
	}
}

func TestReadyPoolCoordinatorRouteUnsupported(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusMethodNotAllowed} {
		if !readyPoolCoordinatorRouteUnsupported(CoordinatorHTTPError{StatusCode: status}) {
			t.Fatalf("status %d was not treated as rollout-compatible", status)
		}
	}
	if readyPoolCoordinatorRouteUnsupported(CoordinatorHTTPError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("server failure was treated as an unsupported route")
	}
}

func TestReadyPoolLegacyEnsureIgnoresUnsupportedCompatibilityCriteria(t *testing.T) {
	ready := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/ready-pools/shared-linux" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		if ready {
			_, _ = w.Write([]byte(`{"pool":[{"key":"shared-linux","leaseID":"cbx_123","state":"ready","owner":"alice@example.com","org":"example-org","repo":"example-org/my-app","ref":"main","createdAt":"2026-05-01T00:00:00Z","updatedAt":"2026-05-01T00:00:00Z","expiresAt":"` + time.Now().Add(time.Hour).Format(time.RFC3339Nano) + `"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"pool":[]}`))
	}))
	defer server.Close()

	client := CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	prewarms := 0
	entries, count, err := ensureReadyPoolLegacy(
		context.Background(),
		&client,
		"shared-linux",
		1,
		true,
		map[string]any{
			"repo":             "example-org/my-app",
			"ref":              "main",
			"compatibilityKey": "linux-16-vcpu",
		},
		func() error {
			prewarms++
			ready = true
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if prewarms != 1 || count != 1 || len(entries) != 1 {
		t.Fatalf("prewarms=%d ready=%d entries=%d", prewarms, count, len(entries))
	}
}

func TestReadyPoolEnsureFallsBackOnceForOlderCoordinator(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/ready-pools/shared-linux/reconcile":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/v1/ready-pools/shared-linux":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"pool":[]}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.readyPoolEnsure(context.Background(), []string{"shared-linux", "--min-ready", "0"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(stderr.String(), "legacy count-then-create fallback"); got != 1 {
		t.Fatalf("fallback notices=%d stderr=%q", got, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "ready=0 min_ready=0") {
		t.Fatalf("fallback output=%q", got)
	}
}

func TestReadyPoolEnsureDoesNotCountAnotherKeepersClaimAsReady(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/ready-pools/shared-linux/reconcile" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"desired":{"key":"shared-linux","criteria":{},"minReady":1,"maxReady":1,"createdAt":"2026-05-01T00:00:00Z","updatedAt":"2026-05-01T00:00:00Z"},"counts":{"ready":0,"busy":0,"draining":0,"quarantined":0,"stale":0,"inFlight":1},"satisfied":false,"reconciling":true,"capped":true,"counters":{}}`))
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "local-test-token")

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.readyPoolEnsure(context.Background(), []string{"shared-linux", "--min-ready", "1", "--create"})
	if err == nil || !strings.Contains(err.Error(), "ready=0") {
		t.Fatalf("pending-only ensure error=%v stdout=%q stderr=%q", err, stdout.String(), stderr.String())
	}
}

func TestValidateReadyPoolEnsurePrewarmArgsRejectsCriteriaOverrides(t *testing.T) {
	if err := validateReadyPoolEnsurePrewarmArgs([]string{"--provider", "aws", "--type", "c6i.2xlarge"}); err != nil {
		t.Fatalf("provider args rejected: %v", err)
	}
	for _, args := range [][]string{
		{"--ref", "release"},
		{"--ref=release"},
		{"--repo", "owner/repo"},
		{"--repo=owner/repo"},
	} {
		if err := validateReadyPoolEnsurePrewarmArgs(args); err == nil {
			t.Fatalf("criteria override %v was accepted", args)
		}
	}
}

func TestReadyPoolRegisterCommitOmitsMismatchedImplicitCommit(t *testing.T) {
	head := strings.Repeat("a", 40)
	other := strings.Repeat("b", 40)
	repo := Repo{Head: head}
	if got := readyPoolRegisterCommit(Config{}, repo, "", ""); got != head {
		t.Fatalf("default register commit=%q, want head", got)
	}
	if got := readyPoolRegisterCommit(Config{}, repo, other, ""); got != "" {
		t.Fatalf("mismatched ref sha registered commit %q", got)
	}
	if got := readyPoolRegisterCommit(Config{}, repo, other, other); got != other {
		t.Fatalf("explicit commit=%q, want other", got)
	}
}

func TestReadyPoolRunReturnDispositionRequiresScrubProof(t *testing.T) {
	runErr := errors.New("remote command exited 1")
	scrubErr := errors.New("scrub failed")
	tests := []struct {
		name            string
		policy          string
		runFailure      error
		scrubErr        error
		wantScrub       bool
		wantResult      string
		metadataMatches bool
	}{
		{name: "success returns after scrub", policy: "auto", wantScrub: true, wantResult: "ready", metadataMatches: true},
		{name: "command failure drains without scrub", policy: "auto", runFailure: runErr, wantResult: "drain", metadataMatches: true},
		{name: "advanced exact entry drains", policy: "auto", wantScrub: true, wantResult: "drain"},
		{name: "transport failure drains", policy: "auto", runFailure: runErr, wantResult: "drain", metadataMatches: true},
		{name: "lifecycle failure drains", policy: "auto", runFailure: runErr, wantResult: "drain", metadataMatches: true},
		{name: "forced ready cannot override lifecycle failure", policy: "ready", runFailure: runErr, wantResult: "drain", metadataMatches: true},
		{name: "forced ready cannot reuse failed command", policy: "ready", runFailure: runErr, scrubErr: scrubErr, wantResult: "drain", metadataMatches: true},
		{name: "explicit drain skips scrub", policy: "drain", wantResult: "drain", metadataMatches: true},
		{name: "explicit release skips scrub", policy: "release", wantResult: "release", metadataMatches: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := readyPoolRunShouldScrub(tc.policy, tc.runFailure); got != tc.wantScrub {
				t.Fatalf("readyPoolRunShouldScrub()=%v, want %v", got, tc.wantScrub)
			}
			if got := readyPoolRunReturnResult(tc.policy, tc.runFailure, tc.scrubErr, tc.metadataMatches); got != tc.wantResult {
				t.Fatalf("readyPoolRunReturnResult()=%q, want %q", got, tc.wantResult)
			}
		})
	}
}

func TestReadyPoolRunReturnReasonPreservesCommandOutcome(t *testing.T) {
	runErr := errors.New("remote command exited 1")
	if got := readyPoolRunReturnReason(runErr, "ready", "abc123", nil, true); got != "run failed; scrubbed for reuse at abc123" {
		t.Fatalf("ready return reason=%q", got)
	}
	if got := readyPoolRunReturnReason(nil, "drain", "", errors.New("scrub failed"), true); got != "pool scrub failed" {
		t.Fatalf("scrub failure reason=%q", got)
	}
	if got := readyPoolRunReturnReason(nil, "drain", "", nil, false); got != "pool hydration or recorded commit is stale" {
		t.Fatalf("advanced commit reason=%q", got)
	}
}

func TestReadyPoolPreparedCommitMatches(t *testing.T) {
	if !readyPoolPreparedCommitMatches("", "new") {
		t.Fatal("ref-only pool entry rejected prepared commit")
	}
	if !readyPoolPreparedCommitMatches("ABC123", "abc123") {
		t.Fatal("same exact commit rejected")
	}
	if readyPoolPreparedCommitMatches("old", "new") {
		t.Fatal("advanced exact-commit entry remained reusable")
	}
}

func TestReadyPoolEntryRequiresHydrationForRefOnlyEntries(t *testing.T) {
	if !readyPoolEntryRequiresHydration(CoordinatorReadyPoolEntry{Ref: "main"}) {
		t.Fatal("ref-only entry skipped hydration proof")
	}
	if readyPoolEntryRequiresHydration(CoordinatorReadyPoolEntry{Ref: "main", Commit: strings.Repeat("a", 40)}) {
		t.Fatal("exact-commit entry unexpectedly required Actions hydration")
	}
	if !readyPoolRunRequiresHydrationProof(CoordinatorReadyPoolEntry{Ref: "main", Commit: strings.Repeat("a", 40)}, true) {
		t.Fatal("exact-commit Actions run skipped hydration proof")
	}
}
