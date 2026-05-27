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

func TestParseMarkedFiles(t *testing.T) {
	files := parseMarkedFiles("\n__CRABBOX_RESULT_FILE__:a.xml\n<a/>\n__CRABBOX_RESULT_FILE__:b.xml\n<b/>\n")
	if files["a.xml"] != "<a/>" || files["b.xml"] != "<b/>" {
		t.Fatalf("files=%#v", files)
	}
}

func TestRemoteFindJUnitResultFiles(t *testing.T) {
	got := remoteFindJUnitResultFiles("/repo", remoteResultsMarker)
	for _, want := range []string{
		"find .",
		"-path './node_modules'",
		"-path '*/node_modules'",
		"-path './.git'",
		"-path '*/.git'",
		"-prune -o -type f",
		"-name 'junit*.xml'",
		"-name 'TEST-*.xml'",
		"-name 'results.xml'",
		"&& { count=0;",
		"[ -f '.crabbox/results-start' ] || exit 0",
		"'.crabbox/results-start' -nt \"$f\"",
		"| sort | while IFS= read -r f",
		"bs=4096 count=1",
		"grep -Eq '<testsuites?'",
		"count=$((count + 1))",
		"bs=1048576 count=1",
		"done; }",
		resultFileMarker,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("auto junit command missing %q:\n%s", want, got)
		}
	}
	for _, blocked := range []string{"-maxdepth", "head -20", "depth=gsub", "-newer", "awk 'NR <="} {
		if strings.Contains(got, blocked) {
			t.Fatalf("auto junit command should be portable, inclusively fresh, and bounded after sniffing, found %q:\n%s", blocked, got)
		}
	}
}

func TestWindowsRemoteFindJUnitResultFilesPrintsPathMarker(t *testing.T) {
	got := decodePowerShellCommand(t, windowsRemoteFindJUnitResultFiles(`C:\repo`, remoteResultsMarker))
	for _, want := range []string{
		"$ErrorActionPreference = \"Stop\"",
		"Set-Location -LiteralPath 'C:\\repo'",
		"$ErrorActionPreference = \"SilentlyContinue\"",
		"function Get-CrabboxJUnitFiles",
		"$_.Name -ne 'node_modules' -and $_.Name -ne '.git'",
		"if (-not (Test-Path -LiteralPath '.crabbox/results-start')) { return }",
		"$markerTime = (Get-Item -LiteralPath '.crabbox/results-start').LastWriteTimeUtc",
		"$_.LastWriteTimeUtc -ge $markerTime",
		"$maxBytes = 1048576",
		"$sniffBytes = 4096",
		"$maxFiles = 50",
		"$prefix -notmatch '<testsuites?'",
		"$count++",
		resultFileMarker + `$($_.FullName)`,
		"[System.IO.File]::OpenRead($_.FullName)",
		"[System.Text.Encoding]::UTF8.GetString($buffer, 0, $read)",
		"[Console]::WriteLine()",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows auto junit command missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, `${($_.FullName)}`) {
		t.Fatalf("windows auto junit command uses variable syntax instead of subexpression:\n%s", got)
	}
	for _, blocked := range []string{"Get-Content -Raw", "Select-Object -First"} {
		if strings.Contains(got, blocked) {
			t.Fatalf("windows auto junit command should cap raw file reads after sniffing, found %q:\n%s", blocked, got)
		}
	}
}

func TestParseAutoJUnitResultsSkipsNonJUnitFiles(t *testing.T) {
	results, err := parseAutoJUnitResults(map[string]string{
		"results.xml": "<not-junit/>",
		"junit.xml":   `<testsuite name="pkg" tests="1" failures="0" errors="0" skipped="0" time="0.1"><testcase name="ok"/></testsuite>`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || results.Tests != 1 || len(results.Files) != 1 || results.Files[0] != "junit.xml" {
		t.Fatalf("unexpected auto results: %#v", results)
	}
}

func TestParseAutoJUnitResultsKeepsFailuresAfterTwentyFiles(t *testing.T) {
	files := map[string]string{}
	for i := 0; i < 25; i++ {
		name := "TEST-pass-" + string(rune('a'+i)) + ".xml"
		files[name] = `<testsuite name="pkg" tests="1" failures="0" errors="0" skipped="0"><testcase name="ok"/></testsuite>`
	}
	files["TEST-z-fail.xml"] = `<testsuite name="pkg" tests="1" failures="1" errors="0" skipped="0"><testcase classname="pkg.Case" name="fails"><failure message="late failure"/></testcase></testsuite>`

	results, err := parseAutoJUnitResults(files)
	if err != nil {
		t.Fatal(err)
	}
	if results == nil || results.Failures != 1 || len(results.Failed) != 1 || results.Failed[0].Message != "late failure" {
		t.Fatalf("late failure was not preserved: %#v", results)
	}
}

func TestNormalizeResultPath(t *testing.T) {
	for _, tc := range []struct {
		workdir string
		name    string
		want    string
	}{
		{workdir: "/repo", name: "./junit.xml", want: "junit.xml"},
		{workdir: "/repo", name: "/repo/reports/junit.xml", want: "reports/junit.xml"},
		{workdir: `C:\work\repo`, name: `C:\work\repo\reports\junit.xml`, want: "reports/junit.xml"},
		{workdir: `C:\work\repo`, name: `c:\work\repo\reports\junit.xml`, want: "reports/junit.xml"},
	} {
		if got := normalizeResultPath(tc.workdir, tc.name); got != tc.want {
			t.Fatalf("normalizeResultPath(%q, %q)=%q, want %q", tc.workdir, tc.name, got, tc.want)
		}
	}
}
