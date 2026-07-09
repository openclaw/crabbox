package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseActionsRunRefFromURL(t *testing.T) {
	ref, err := parseActionsRunRef("https://github.com/example-org/my-app/actions/runs/123456/attempts/2", "")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repo.Slug() != "example-org/my-app" || ref.RunID != "123456" || ref.Attempt != 2 {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestParseActionsRunRefRequiresRepoForNumericID(t *testing.T) {
	if _, err := parseActionsRunRef("123456", ""); err == nil {
		t.Fatal("expected numeric run id without --repo to fail")
	}
	ref, err := parseActionsRunRef("123456", "example-org/my-app")
	if err != nil {
		t.Fatal(err)
	}
	if ref.Repo.Slug() != "example-org/my-app" || ref.RunID != "123456" {
		t.Fatalf("unexpected ref: %#v", ref)
	}
}

func TestParseActionsRunRefRejectsInvalidIDsAndAttempts(t *testing.T) {
	for _, value := range []string{
		"0",
		"-1",
		"https://github.com/example-org/my-app/actions/runs/not-a-number",
		"https://github.com/example-org/my-app/actions/runs/123/attempts/0",
		"https://github.com/example-org/my-app/actions/runs/123/attempts/nope",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := parseActionsRunRef(value, "example-org/my-app"); err == nil {
				t.Fatal("expected invalid run ref to fail")
			}
		})
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
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123"}
	view := capsuleRunView{
		URL:          "https://github.com/example-org/my-app/actions/runs/123",
		Attempt:      2,
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
	if manifest.Oracle.FailureSignature != "FAIL" || !strings.Contains(manifest.Oracle.SuccessCondition, "same failure signature") {
		t.Fatalf("unexpected oracle: %#v", manifest.Oracle)
	}
	if manifest.Safety.ActionProfile != "build_debug_v1" || manifest.Extensions[repoBuildReplayClass] == nil {
		t.Fatalf("foundation fields missing: %#v", manifest)
	}
	if manifest.Source.Attempt != 2 || !strings.Contains(manifest.Inputs.ActionsRunDigest, "attempt-2") {
		t.Fatalf("attempt was not resolved into manifest identity: source=%#v inputs=%#v", manifest.Source, manifest.Inputs)
	}
}

func TestBuildActionsCapsuleManifestAllowsExitOnlyOracle(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123"}
	manifest := buildActionsCapsuleManifest(ref, capsuleRunView{}, "", capsuleJobView{Name: "Go"}, capsuleStepView{Name: "Test"}, "Replay", "go test ./...", "", "", capsuleArtifactRef{}, nil)
	if manifest.Oracle.FailureSignature != "" {
		t.Fatalf("unexpected fallback failure signature: %#v", manifest.Oracle)
	}
	if manifest.Oracle.SuccessCondition != "The replay command exits non-zero." {
		t.Fatalf("unexpected success condition: %q", manifest.Oracle.SuccessCondition)
	}
	if manifest.Replay.NondeterminismBudget != "exit code must remain non-zero" {
		t.Fatalf("unexpected nondeterminism budget: %q", manifest.Replay.NondeterminismBudget)
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

func TestCapsuleFilesRepairPrivatePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "capsule")
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, logsDir} {
		if err := os.Chmod(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	logPath := filepath.Join(logsDir, "failed.log")
	manifestPath := filepath.Join(dir, capsuleManifestFileName)
	for _, path := range []string{logPath, manifestPath} {
		if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensurePrivateCapsuleDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePrivateCapsuleDir(logsDir); err != nil {
		t.Fatal(err)
	}
	if err := writePrivateCapsuleFile(logPath, []byte("secret log")); err != nil {
		t.Fatal(err)
	}
	if err := writeCapsuleManifest(manifestPath, capsuleManifest{CapsuleVersion: capsuleVersion}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{dir, logsDir} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != privateCapsuleDirMode {
			t.Fatalf("directory %s mode=%#o, want %#o", path, got, privateCapsuleDirMode)
		}
	}
	for _, path := range []string{logPath, manifestPath} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != privateCapsuleFileMode {
			t.Fatalf("file %s mode=%#o, want %#o", path, got, privateCapsuleFileMode)
		}
	}
}

func TestWriteCapsuleManifestDoesNotChangeExistingParentPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not enforced on Windows")
	}
	dir := filepath.Join(t.TempDir(), "shared")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, capsuleManifestFileName)
	if err := writeCapsuleManifest(path, capsuleManifest{CapsuleVersion: capsuleVersion}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("existing parent mode=%#o, want unchanged 0755", got)
	}
	info, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != privateCapsuleFileMode {
		t.Fatalf("manifest mode=%#o, want %#o", got, privateCapsuleFileMode)
	}
}

func TestCapsuleFailureSignatureUsesLastLogLine(t *testing.T) {
	got := capsuleFailureSignature("Go\tTest\tfirst\nGo\tTest\tpanic: broken\nGo\tTest\tCleaning up orphan processes\n")
	if got != "panic: broken" {
		t.Fatalf("signature=%q", got)
	}
}

func TestCapsuleFailureSignatureStripsGitHubPrefixes(t *testing.T) {
	got := capsuleFailureSignature("Plugin\tCheck\t2026-05-16T05:20:06.2281757Z Error: missing plugin\nPlugin\tCheck\t2026-05-16T05:20:07.0000000Z ##[error]Process completed with exit code 1.\n")
	if got != "Error: missing plugin" {
		t.Fatalf("signature=%q", got)
	}
}

func TestCapsuleFailureSignatureStripsShortGitHubTimestamp(t *testing.T) {
	got := capsuleFailureSignature("Plugin\tCheck\t2026-01-01T00:00:00Z Error\n")
	if got != "Error" {
		t.Fatalf("signature=%q", got)
	}
}

func TestCapsuleFailureSignatureForSelectionFiltersJobAndStep(t *testing.T) {
	log := strings.Join([]string{
		"Go\tTest\tpanic: selected",
		"Worker\tCheck\tpanic: other",
	}, "\n")
	got := capsuleFailureSignatureForSelection(log, "Go", "Test")
	if got != "panic: selected" {
		t.Fatalf("signature=%q", got)
	}
	if got := capsuleFailureSignatureForSelection(log, "Docs", "Test"); got != "" {
		t.Fatalf("unexpected unmatched signature=%q", got)
	}
}

func TestCapsuleFailureSignatureForSelectionFallsBackToJob(t *testing.T) {
	log := strings.Join([]string{
		"Go\tUNKNOWN STEP\t\ufeff2026-05-16T05:20:06.2281757Z Error: selected",
		"Worker\tUNKNOWN STEP\t2026-05-16T05:20:07.0000000Z Error: other",
	}, "\n")
	got := capsuleFailureSignatureForSelection(log, "Go", "Test")
	if got != "Error: selected" {
		t.Fatalf("signature=%q", got)
	}
}

func TestCapsuleFailureSignatureSkipsGenericFailSummaries(t *testing.T) {
	got := capsuleFailureSignature("Go\tTest\tassertion failed: wanted true\nGo\tTest\tFAIL\tgithub.com/example-org/my-app\t0.1s\n")
	if got != "assertion failed: wanted true" {
		t.Fatalf("signature=%q", got)
	}
}

func TestCapsuleFailureSignaturePreservesTabbedMessages(t *testing.T) {
	got := capsuleFailureSignature("Go\tTest\tError:\twant\tgot\n")
	if got != "Error:\twant\tgot" {
		t.Fatalf("signature=%q", got)
	}
}

func TestSafePathComponent(t *testing.T) {
	got := safePathComponent("Example Org/My App Actions 123")
	if strings.ContainsAny(got, "/ ") || got != "example-org-my-app-actions-123" {
		t.Fatalf("safe component=%q", got)
	}
}

func TestDefaultCapsuleOutputNameIncludesAttempt(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123", Attempt: 2}
	if got := defaultCapsuleOutputName(ref, capsuleJobView{}, capsuleStepView{}, false); got != "example-org-my-app-actions-123-attempt-2" {
		t.Fatalf("output name=%q", got)
	}
}

func TestDefaultCapsuleOutputNameOmitsFirstAttempt(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123", Attempt: 1}
	if got := defaultCapsuleOutputName(ref, capsuleJobView{}, capsuleStepView{}, false); got != "example-org-my-app-actions-123" {
		t.Fatalf("output name=%q", got)
	}
}

func TestDefaultCapsuleOutputNameDisambiguatesSelectedFailure(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123", Attempt: 1}
	got := defaultCapsuleOutputName(ref, capsuleJobView{Name: "Windows"}, capsuleStepView{Name: "Test"}, true)
	if got != "example-org-my-app-actions-123-windows-test" {
		t.Fatalf("output name=%q", got)
	}
}

func TestCapsuleIDDigestIncludesAttempt(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123", Attempt: 1}
	one := capsuleIDDigest(ref, "abc", "go test ./...", "Go\nTest\nFAIL")
	ref.Attempt = 2
	two := capsuleIDDigest(ref, "abc", "go test ./...", "Go\nTest\nFAIL")
	if one == two {
		t.Fatal("attempt-specific captures should not share capsule ids")
	}
}

func TestCapsuleIDDigestIncludesSelectedFailure(t *testing.T) {
	ref := actionsRunRef{Repo: GitHubRepo{Owner: "example-org", Name: "my-app"}, RunID: "123", Attempt: 1}
	one := capsuleIDDigest(ref, "abc", "go test ./...", "Go\nTest\nFAIL")
	two := capsuleIDDigest(ref, "abc", "go test ./...", "Windows\nTest\nFAIL")
	if one == two {
		t.Fatal("different selected failures should not share capsule ids")
	}
}

func TestAppendActionsArtifactRefsSkipsExpiredAndPreservesPages(t *testing.T) {
	got := appendActionsArtifactRefs(nil, []actionsArtifact{
		{Name: "page-1", SizeInBytes: 10, ArchiveDownloadURL: "https://example.com/1"},
		{Name: "expired", Expired: true, SizeInBytes: 20, ArchiveDownloadURL: "https://example.com/expired"},
	})
	got = appendActionsArtifactRefs(got, []actionsArtifact{
		{Name: "page-2", SizeInBytes: 30, ArchiveDownloadURL: "https://example.com/2"},
	})
	if len(got) != 2 || got[0].Name != "page-1" || got[1].Name != "page-2" {
		t.Fatalf("artifacts=%#v", got)
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
