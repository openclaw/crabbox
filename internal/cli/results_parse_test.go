package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/openclaw/crabbox/internal/runner"
	"github.com/openclaw/crabbox/internal/runner/runnerfs"
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

func TestParseJUnitResultsNested(t *testing.T) {
	for _, counters := range []string{` tests="1" failures="1" time="0.25"`, "", ` tests="0" failures="0" errors="0" skipped="0"`} {
		for _, root := range []string{"testsuite", "testsuites"} {
			t.Run(root+"/"+counters, func(t *testing.T) {
				report := `<testsuite name="project"` + counters + `><testsuite name="auth"` + counters + `><testcase name="testLogin" classname="AuthTest" file="tests/AuthTest.php" time="0.25"><failure type="AssertionError" message="expected 200, got 401">details</failure></testcase></testsuite></testsuite>`
				if root == "testsuites" {
					report = `<testsuites tests="1" failures="1" time="0.25">` + report + `</testsuites>`
				}
				got, err := parseJUnitResults(map[string]string{"junit.xml": report})
				if err != nil {
					t.Fatal(err)
				}
				want := &TestResultSummary{Format: "junit", Files: []string{"junit.xml"}, Suites: 2, Tests: 1, Failures: 1, TimeSeconds: 0.25,
					Failed: []TestFailure{{Suite: "auth", Name: "testLogin", Classname: "AuthTest", File: "tests/AuthTest.php", Message: "expected 200, got 401", Type: "AssertionError", Kind: "failure"}}}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("nested summary=%+v, want %+v", got, want)
				}
				if !failRunForTestResults(0, ResultsConfig{FailOnFailures: true}, got) {
					t.Error("nested failure did not reject a zero-exit command")
				}
				if failRunForTestResults(23, ResultsConfig{FailOnFailures: true}, got) {
					t.Error("nested failure replaced authoritative command exit")
				}
			})
		}
	}
}

func TestParseJUnitResultsNestedAggregation(t *testing.T) {
	const children = `<testcase name="direct" time="0.25"><failure message=" direct message ">ignored text</failure></testcase>
<testsuite name="branch"><testsuite name="leaf" tests="3" errors="1" skipped="1" time="1.5">
<testcase name="broken" classname="Example" file="example_test.go" time="0.5"><error type="RuntimeError"> error text </error></testcase>
<testcase name="skipped" time="0.25"><skipped/></testcase><testcase name="passed" time="0.75"/>
</testsuite></testsuite><testsuite name="sibling"><testcase name="last" time="0.25"><failure message=" "> last text </failure></testcase></testsuite>`
	for _, tc := range []struct {
		name, attrs                      string
		tests, failures, errors, skipped int
		time                             float64
	}{
		{"derived", "", 5, 2, 1, 1, 2},
		{"aggregate", ` tests="5" failures="2" errors="1" skipped="1" time="2"`, 5, 2, 1, 1, 2},
		{"partial", ` tests="5" time="3"`, 5, 2, 1, 1, 3},
		{"zero", ` tests="0" failures="0" errors="0" skipped="0" time="0"`, 5, 2, 1, 1, 2},
		{"omitted tests", ` failures="4" errors="3" skipped="2" time="4"`, 5, 4, 3, 2, 4},
		{"observed lower bounds", ` tests="1" failures="1" errors="0" skipped="0"`, 1, 2, 1, 1, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJUnitResults(map[string]string{"junit.xml": `<testsuite name="parent"` + tc.attrs + `>` + children + `</testsuite>`})
			if err != nil {
				t.Fatal(err)
			}
			want := &TestResultSummary{Format: "junit", Files: []string{"junit.xml"}, Suites: 4, Tests: tc.tests, Failures: tc.failures, Errors: tc.errors, Skipped: tc.skipped, TimeSeconds: tc.time,
				Failed: []TestFailure{
					{Suite: "parent", Name: "direct", Message: "direct message", Kind: "failure"},
					{Suite: "leaf", Name: "broken", Classname: "Example", File: "example_test.go", Message: "error text", Type: "RuntimeError", Kind: "error"},
					{Suite: "sibling", Name: "last", Message: "last text", Kind: "failure"},
				}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("summary=%+v, want %+v", got, want)
			}
		})
	}
}

func TestParseJUnitResultsNestedFilesAndMalformedSibling(t *testing.T) {
	got, err := parseJUnitResults(map[string]string{
		"z.xml":   `<testsuites><testsuite name="z"><testsuite name="leaf"><testcase name="z"><failure message="last"/></testcase></testsuite></testsuite><testsuite name="sibling" tests="2" skipped="2" time="0.5"/></testsuites>`,
		"bad.xml": `<testsuite tests="99"><testsuite><testcase name="partial"><failure/></testcase></testsuite>`,
		"a.xml":   `<testsuite name="a"><testcase name="a"><failure message="first"/><failure>second</failure><error>third</error></testcase></testsuite>`,
	})
	if err == nil || !strings.Contains(err.Error(), "skip junit bad.xml") {
		t.Fatalf("missing malformed-file warning: %v", err)
	}
	want := &TestResultSummary{Format: "junit", Files: []string{"a.xml", "z.xml"}, Suites: 4, Tests: 4, Failures: 3, Errors: 1, Skipped: 2, TimeSeconds: 0.5,
		Failed: []TestFailure{
			{Suite: "a", Name: "a", Message: "first", Kind: "failure"},
			{Suite: "a", Name: "a", Message: "second", Kind: "failure"},
			{Suite: "a", Name: "a", Message: "third", Kind: "error"},
			{Suite: "leaf", Name: "z", Message: "last", Kind: "failure"},
		}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary=%+v, want %+v", got, want)
	}
}

func TestParseJUnitResultsFlatAccounting(t *testing.T) {
	for _, tc := range []struct {
		name, report                             string
		suites, tests, failures, errors, skipped int
		time                                     float64
	}{
		{"reported", `<testsuite tests="7" failures="3" errors="2" skipped="1" time="4"><testcase time="0.25"/></testsuite>`, 1, 7, 3, 2, 1, 4},
		{"derived", `<testsuite><testcase time="0.25"><failure/><error/><skipped/></testcase></testsuite>`, 1, 1, 1, 1, 1, 0.25},
		{"zero counters", `<testsuite tests="0" failures="0" errors="0" skipped="0"><testcase time="0.25"><failure/><error/><skipped/></testcase></testsuite>`, 1, 1, 1, 1, 1, 0.25},
		{"time without counters", `<testsuite time="4"><testcase time="0.25"/></testsuite>`, 1, 1, 0, 0, 0, 0.25},
		{"wrapper aggregates", `<testsuites tests="99" failures="99" time="99"><testsuite tests="2" time="0.5"/><testsuite tests="3" time="1"/></testsuites>`, 2, 5, 0, 0, 0, 1.5},
		{"summary only", `<testsuites tests="7" failures="3" errors="2" skipped="1" time="4"/>`, 0, 7, 3, 2, 1, 4},
		{"empty wrapper", `<testsuites/>`, 0, 0, 0, 0, 0, 0},
		{"empty suite", `<testsuite/>`, 1, 0, 0, 0, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseJUnitResults(map[string]string{"junit.xml": tc.report})
			if err != nil {
				t.Fatal(err)
			}
			if got.Suites != tc.suites || got.Tests != tc.tests || got.Failures != tc.failures || got.Errors != tc.errors || got.Skipped != tc.skipped || got.TimeSeconds != tc.time {
				t.Fatalf("summary=%+v, want %+v", got, tc)
			}
		})
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

func junitFixtureClient() *runner.Client {
	identity := runner.Identity{BuildID: "junit-fixture", OS: runtime.GOOS, Arch: runtime.GOARCH, Protocol: 1}
	return &runner.Client{Identity: identity, Transport: func(ctx context.Context, input io.Reader, output io.Writer) error {
		return runner.Serve(ctx, input, output, identity)
	}}
}

func TestCollectRemoteJUnitResultsDeduplicatesExplicitAliases(t *testing.T) {
	workdir := t.TempDir()
	const report = `<testsuite name="sample" tests="1" failures="1"><testcase name="fails"><failure message="boom"/></testcase></testsuite>`
	for _, name := range []string{"junit.xml", "other.xml"} {
		writeFile(t, filepath.Join(workdir, name), report)
	}
	abs := filepath.Join(workdir, "junit.xml")
	for _, tc := range []struct {
		name      string
		paths     []string
		auto      bool
		wantFiles []string
	}{
		{"relative first", []string{"junit.xml", "./junit.xml", abs}, false, []string{"junit.xml"}},
		{"absolute first", []string{abs, "./junit.xml", "junit.xml"}, false, []string{abs}},
		{"explicit and auto", []string{"junit.xml", "./junit.xml", abs}, true, []string{"junit.xml"}},
		{"identical reports remain distinct", []string{"junit.xml", "other.xml"}, false, []string{"junit.xml", "other.xml"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := CollectJUnitResultsWithRunner(t.Context(), junitFixtureClient(), workdir, ResultsConfig{JUnit: tc.paths, Auto: tc.auto}, "")
			if err != nil || len(got.Warnings) != 0 {
				t.Fatalf("collection=%+v err=%v", got, err)
			}
			count := len(tc.wantFiles)
			if got.Summary == nil || !reflect.DeepEqual(got.Summary.Files, tc.wantFiles) || got.Summary.Tests != count || got.Summary.Failures != count {
				t.Fatalf("summary=%+v want files=%v", got.Summary, tc.wantFiles)
			}
		})
	}
	t.Run("opened file aliases", func(t *testing.T) {
		if err := os.Link(abs, filepath.Join(workdir, "same.xml")); err != nil {
			t.Skipf("hardlink unavailable: %v", err)
		}
		got, err := CollectJUnitResultsWithRunner(t.Context(), junitFixtureClient(), workdir, ResultsConfig{JUnit: []string{"junit.xml", "same.xml"}, Auto: true}, "")
		if err != nil || got.Summary == nil || len(got.Summary.Files) != 1 {
			t.Fatalf("collection=%+v err=%v", got, err)
		}
	})
}

func TestRunnerResultFilesPreservesOriginalBytes(t *testing.T) {
	data := []byte("\r\n<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>\r\n<testsuite name=\"caf\xe9\"/>\r\n")
	files, warnings := runnerResultFiles(runnerfs.Results{Files: []runnerfs.File{{Path: "junit.xml", Data: data}}, Warnings: []runnerfs.Warning{{Path: "large.xml", Message: "report exceeds limit"}}})
	if files["junit.xml"] != string(data) {
		t.Fatal("report bytes changed")
	}
	if len(warnings) != 1 || warnings[0].Error() != "skip junit large.xml: report exceeds limit" {
		t.Fatalf("warnings=%v", warnings)
	}
}

func TestCollectJUnitResultsWithRunnerRetainsValidReportsAndWarnings(t *testing.T) {
	workdir := t.TempDir()
	writeFile(t, filepath.Join(workdir, "junit.xml"), `<testsuite tests="1"/>`)
	writeFile(t, filepath.Join(workdir, "broken.xml"), `<testsuite`)
	got, err := CollectJUnitResultsWithRunner(t.Context(), junitFixtureClient(), workdir, ResultsConfig{JUnit: []string{"junit.xml", "broken.xml"}}, "")
	if err != nil || got.Summary == nil || got.Summary.Tests != 1 || len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0].Error(), "broken.xml") {
		t.Fatalf("collection=%+v err=%v", got, err)
	}
	failed := junitFixtureClient()
	failed.Transport = func(context.Context, io.Reader, io.Writer) error { return errors.New("fixture unavailable") }
	got, err = CollectJUnitResultsWithRunner(t.Context(), failed, workdir, ResultsConfig{JUnit: []string{"junit.xml"}}, "")
	if err == nil || got.Summary != nil {
		t.Fatalf("collection=%+v err=%v", got, err)
	}
}

func TestCollectJUnitResultsWithRunnerAutoKeepsFailuresAfterTwentyFiles(t *testing.T) {
	workdir := t.TempDir()
	for i := 0; i < 25; i++ {
		writeFile(t, filepath.Join(workdir, fmt.Sprintf("TEST-%02d.xml", i)), `<testsuite tests="1"/>`)
	}
	writeFile(t, filepath.Join(workdir, "junit-failing.xml"), `<testsuite tests="1" failures="1"><testcase name="fails"><failure/></testcase></testsuite>`)
	writeFile(t, filepath.Join(workdir, "results.xml"), `<settings/>`)
	got, err := CollectJUnitResultsWithRunner(t.Context(), junitFixtureClient(), workdir, ResultsConfig{Auto: true}, "")
	if err != nil || got.Summary == nil || got.Summary.Tests != 26 || got.Summary.Failures != 1 || len(got.Summary.Files) != 26 {
		t.Fatalf("collection=%+v err=%v", got, err)
	}
}
