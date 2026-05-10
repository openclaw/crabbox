package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseActionsRunRefFromURL(t *testing.T) {
	ref, err := parseActionsRunRef("https://github.com/openclaw/crabbox/actions/runs/123456/attempts/2", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repo.Slug() != "openclaw/crabbox" || ref.RunID != "123456" || ref.Attempt != 2 {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestParseActionsRunRefRequiresRepoForNumericID(t *testing.T) {
	if _, err := parseActionsRunRef("123456", ""); err == nil {
		t.Fatal("expected numeric run id without --repo to fail")
	}
	ref, err := parseActionsRunRef("123456", "openclaw/crabbox")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repo.Slug() != "openclaw/crabbox" || ref.RunID != "123456" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestSelectCapsuleFailurePrefersFailedJobAndStep(t *testing.T) {
	job, step, matched := selectCapsuleFailure([]capsuleJobView{
		{Name: "Docs", Conclusion: "success"},
		{Name: "Go", Conclusion: "failure", Steps: []capsuleStepView{
			{Name: "Set up", Conclusion: "success"},
			{Name: "Test", Conclusion: "failure"},
		}},
	}, "")
	if !matched || job.Name != "Go" || step.Name != "Test" {
		t.Fatalf("matched=%t job=%#v step=%#v", matched, job, step)
	}
}

func TestSelectCapsuleFailureReportsMissingPreferredJob(t *testing.T) {
	job, step, matched := selectCapsuleFailure([]capsuleJobView{
		{Name: "Docs", Conclusion: "success"},
		{Name: "Go", Conclusion: "failure", Steps: []capsuleStepView{{Name: "Test", Conclusion: "failure"}}},
	}, "Windows")
	if matched {
		t.Fatalf("matched missing job with job=%#v step=%#v", job, step)
	}
}

func TestBuildActionsCapsuleManifestKeepsSmallContract(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "openclaw", Name: "crabbox"}, RunID: "123"}
	view := capsuleRunView{
		URL:          "https://github.com/openclaw/crabbox/actions/runs/123",
		WorkflowName: "CI",
		HeadSHA:      "abc123",
		Conclusion:   "failure",
	}
	job := capsuleJobView{Name: "Go", Conclusion: "failure"}
	step := capsuleStepView{Name: "Test", Conclusion: "failure"}
	manifest := buildActionsCapsuleManifest(ref, view, ".github/workflows/ci.yml", job, step, "Replay CI Go Test", "go test ./...", "semantically_identical", "FAIL", capsuleArtifactRef{}, nil)
	if manifest.Class != repoBuildReplayClass || manifest.Source.Kind != "github_actions" {
		t.Fatalf("unexpected class/source: %#v", manifest)
	}
	if manifest.Replay.Command != "go test ./..." || manifest.Replay.CommandMode != "shell" {
		t.Fatalf("unexpected replay: %#v", manifest.Replay)
	}
	if manifest.Safety.ActionProfile != "build_debug_v1" || manifest.Extensions[repoBuildReplayClass] == nil {
		t.Fatalf("foundation fields missing: %#v", manifest)
	}
}

func TestCapsuleManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, capsuleManifestFileName)
	manifest := capsuleManifest{
		CapsuleVersion: capsuleVersion,
		CapsuleID:      "sha256:test",
		Class:          repoBuildReplayClass,
		ClassVersion:   repoBuildReplayVersion,
		Scenario:       "test",
		Replay:         capsuleReplayContract{Command: "go test ./...", CommandMode: "shell", RequiredQuality: "semantically_identical"},
	}
	if err := writeCapsuleManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	got, err := readCapsuleManifest(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.CapsuleID != manifest.CapsuleID || got.Replay.Command != manifest.Replay.Command {
		t.Fatalf("got=%#v want=%#v", got, manifest)
	}
}

func TestCapsuleFailureSignatureUsesLastLogLine(t *testing.T) {
	got := capsuleFailureSignature("Go\tTest\tfirst\nGo\tTest\tpanic: broken\nGo\tTest\tCleaning up orphan processes\n")
	if got != "panic: broken" {
		t.Fatalf("signature=%q", got)
	}
}

func TestSafePathComponent(t *testing.T) {
	got := safePathComponent("OpenClaw/Crabbox Actions 123")
	if strings.ContainsAny(got, "/ ") || got != "openclaw-crabbox-actions-123" {
		t.Fatalf("safe component=%q", got)
	}
}

func TestRemoteReplayExitCodeClassifiesExpectedFailure(t *testing.T) {
	tests := []struct {
		message string
		want    int
	}{
		{message: "remote command exited 17", want: 17},
		{message: "blacksmith testbox run exited 23", want: 23},
		{message: "islo run exited 4", want: 4},
		{message: "delegated provider command exited 5", want: 5},
	}
	for _, tt := range tests {
		t.Run(tt.message, func(t *testing.T) {
			code, ok := remoteReplayExitCode(ExitError{Code: tt.want, Message: tt.message})
			if !ok || code != tt.want {
				t.Fatalf("code=%d ok=%t want=%d", code, ok, tt.want)
			}
		})
	}
}

func TestRemoteReplayExitCodeRejectsConfigAndProviderErrors(t *testing.T) {
	for _, message := range []string{
		"missing config",
		"blacksmith failed: exit status 1",
		"e2b run failed: process failed before command",
		"e2b run failed: setup command exited 1",
	} {
		t.Run(message, func(t *testing.T) {
			if _, ok := remoteReplayExitCode(ExitError{Code: 2, Message: message}); ok {
				t.Fatalf("configuration/provider error %q should not be treated as reproduced failure", message)
			}
		})
	}
}

func TestCapsuleReplayFailureOutcomeChecksSignature(t *testing.T) {
	outcome, note, reproduced := capsuleReplayFailureOutcome("panic: broken", "setup\npanic: broken\n", 2)
	if !reproduced || outcome != capsuleOutcomeFailReproduced || !strings.Contains(note, "matched failure_signature") {
		t.Fatalf("outcome=%s reproduced=%t note=%q", outcome, reproduced, note)
	}

	outcome, note, reproduced = capsuleReplayFailureOutcome("panic: broken", "different failure\n", 2)
	if reproduced || outcome != capsuleOutcomeFailNew || !strings.Contains(note, "failure_signature was not present") {
		t.Fatalf("outcome=%s reproduced=%t note=%q", outcome, reproduced, note)
	}

	outcome, _, reproduced = capsuleReplayFailureOutcome("", "", 2)
	if !reproduced || outcome != capsuleOutcomeFailReproduced {
		t.Fatalf("blank signature outcome=%s reproduced=%t", outcome, reproduced)
	}
}
