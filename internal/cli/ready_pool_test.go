package cli

import (
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
