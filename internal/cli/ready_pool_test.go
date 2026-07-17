package cli

import (
	"errors"
	"strings"
	"testing"
	"time"
)

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

func TestCountReadyPoolEntriesUsesBorrowCriteria(t *testing.T) {
	future := time.Now().Add(time.Hour).Format(time.RFC3339Nano)
	entries := []CoordinatorReadyPoolEntry{
		{State: "ready", ExpiresAt: future, Repo: "openclaw/openclaw", Ref: "main", Commit: "aaa"},
		{State: "ready", ExpiresAt: future, Repo: "openclaw/openclaw", Ref: "main", Commit: "bbb"},
		{State: "ready", ExpiresAt: future, Repo: "openclaw/openclaw", Ref: "main"},
		{State: "busy", ExpiresAt: future, Repo: "openclaw/openclaw", Ref: "main", Commit: "aaa"},
		{State: "ready", ExpiresAt: future, Repo: "openclaw/openclaw", Ref: "release", Commit: "aaa"},
	}

	strict := map[string]any{"repo": "openclaw/openclaw", "ref": "main", "commit": "aaa"}
	if got := countReadyPoolEntries(entries, strict); got != 1 {
		t.Fatalf("strict ready count=%d, want 1", got)
	}

	branch := map[string]any{"repo": "openclaw/openclaw", "ref": "main", "commit": "aaa", "allowMissingCommit": true}
	if got := countReadyPoolEntries(entries, branch); got != 2 {
		t.Fatalf("branch ready count=%d, want 2", got)
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

	_, err = readyPoolRunBorrowInputForRun(Config{Actions: ActionsConfig{Ref: "feature"}}, Repo{BaseRef: "main"}, "openclaw/openclaw", true)
	if err == nil {
		t.Fatal("no-sync ref-only input succeeded")
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
