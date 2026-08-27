package cli

import (
	"strings"
	"testing"
)

func TestParseJUnitResults(t *testing.T) {
	results, err := parseJUnitResults(map[string]string{"junit.xml": `<testsuite name="pkg" tests="2" failures="1" errors="0" skipped="0" time="1.5">
<testcase classname="pkg.TestThing" name="passes"/>
<testcase classname="pkg.TestThing" name="fails" file="thing_test.go"><failure message="want ok">details</failure></testcase>
</testsuite>`})
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || results.Tests != 2 || results.Failures != 1 || results.Errors != 0 || len(results.Failed) != 1 {
		t.Fatalf("unexpected results: %#v", results)
	}
	if results.Failed[0].Name != "fails" || results.Failed[0].File != "thing_test.go" {
		t.Fatalf("unexpected failure: %#v", results.Failed[0])
	}
}

func TestParseJUnitResultsInitializesEmptyFailureList(t *testing.T) {
	results, err := parseJUnitResults(map[string]string{"junit.xml": `<testsuite name="pkg" tests="1" failures="0" errors="0" skipped="0" time="0.1">
<testcase classname="pkg.TestThing" name="passes"/>
</testsuite>`})
	if err != nil {
		t.Fatal(err)
	}
	if results == nil {
		t.Fatal("results nil")
	}
	if results.Failed == nil {
		t.Fatalf("failed slice is nil: %#v", results)
	}
	if len(results.Failed) != 0 {
		t.Fatalf("failed=%#v", results.Failed)
	}
}

func TestParseJUnitResultsPreservesValidFilesWhenAnotherIsMalformed(t *testing.T) {
	results, err := parseJUnitResults(map[string]string{
		"good.xml": `<testsuite name="pkg" tests="1" failures="1"><testcase name="fails"><failure message="boom"/></testcase></testsuite>`,
		"bad.xml":  `<testsuite name="partial"><testcase`,
	})
	if err == nil || !strings.Contains(err.Error(), "skip junit bad.xml") {
		t.Fatalf("error=%v, want named malformed-file warning", err)
	}
	if results == nil || results.Tests != 1 || results.Failures != 1 || len(results.Files) != 1 || results.Files[0] != "good.xml" {
		t.Fatalf("valid results were not preserved: %#v", results)
	}
}

func TestParseJUnitResultsAcceptsReportsLargerThanFormerAutoLimit(t *testing.T) {
	padding := strings.Repeat("x", (1<<20)+1)
	results, err := parseJUnitResults(map[string]string{
		"large.xml": `<testsuite name="large" tests="1"><testcase name="ok"/><system-out>` + padding + `</system-out></testsuite>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || results.Tests != 1 || len(results.Files) != 1 {
		t.Fatalf("large report was not parsed: %#v", results)
	}
}

func TestParseJUnitResultsDerivesFailuresWhenSuiteCountersAreOmitted(t *testing.T) {
	results, err := parseJUnitResults(map[string]string{
		"junit.xml": `<testsuite name="pkg" tests="1"><testcase name="fails"><failure message="boom"/></testcase></testsuite>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || results.Failures != 1 || len(results.Failed) != 1 {
		t.Fatalf("testcase failure was not reflected in aggregate counters: %#v", results)
	}
}

func TestRemoteTouchResultsMarkerUsesGitMetadataWhenAvailable(t *testing.T) {
	got := remoteTouchResultsMarker("/repo")
	for _, want := range []string{
		"cd '/repo'",
		"marker=.crabbox/results-start",
		"git rev-parse --git-path 'crabbox/results-start'",
		"marker=$git_marker",
		"mkdir -p \"$(dirname \"$marker\")\"",
		": > \"$marker\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("touch marker command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "mkdir -p .git") || strings.Contains(got, ".git/crabbox/results-start") {
		t.Fatalf("touch marker command should not hard-code .git:\n%s", got)
	}
}

func TestWindowsRemoteTouchResultsMarkerUsesGitMetadataWhenAvailable(t *testing.T) {
	got := decodePowerShellCommand(t, windowsRemoteTouchResultsMarker(`C:\repo`))
	for _, want := range []string{
		"Set-Location -LiteralPath 'C:\\repo'",
		"$marker = '.crabbox/results-start'",
		"Get-Command git -ErrorAction SilentlyContinue",
		"git rev-parse --git-path 'crabbox/results-start'",
		"$marker = ([string]$gitMarker).Trim()",
		"New-Item -ItemType Directory -Force -Path $markerDir",
		"Set-Content -LiteralPath $marker",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows touch marker command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "New-Item -ItemType Directory -Force -Path .git/crabbox") || strings.Contains(got, ".git/crabbox/results-start") {
		t.Fatalf("windows touch marker command should not hard-code .git:\n%s", got)
	}
}

func TestFailRunForTestResultsIsOptInAndPreservesCommandFailure(t *testing.T) {
	failing := &TestResultSummary{Failures: 1}
	if failRunForTestResults(0, ResultsConfig{}, failing) {
		t.Fatal("test failures changed exit status without opt-in")
	}
	if !failRunForTestResults(0, ResultsConfig{FailOnFailures: true}, failing) {
		t.Fatal("opt-in test failure did not change successful command status")
	}
	if failRunForTestResults(7, ResultsConfig{FailOnFailures: true}, failing) {
		t.Fatal("test result policy must not replace a command failure")
	}
	if !failRunForTestResults(0, ResultsConfig{FailOnFailures: true}, &TestResultSummary{Failed: []TestFailure{{Name: "case"}}}) {
		t.Fatal("parsed failed cases must fail the run even when aggregate counters are missing")
	}
}
