package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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

func TestParseMarkedFiles(t *testing.T) {
	files := parseMarkedFiles("\n__CRABBOX_RESULT_FILE__:a.xml\n<a/>\n__CRABBOX_RESULT_FILE__:b.xml\n<b/>\n")
	if files["a.xml"] != "<a/>" || files["b.xml"] != "<b/>" {
		t.Fatalf("files=%#v", files)
	}
}

func TestParseMarkedResultOutputReportsBoundedAutoSkips(t *testing.T) {
	files, warnings := parseMarkedResultOutput("\n" + resultFileMarker + "good.xml\n<testsuite/>\n" + resultWarningMarker + "huge.xml\treport exceeds 16777216-byte per-file limit\n")
	if files["good.xml"] != "<testsuite/>" {
		t.Fatalf("files=%#v", files)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0].Error(), "skip junit huge.xml") {
		t.Fatalf("warnings=%#v", warnings)
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
		"&& { tmp=$(mktemp) || exit 0;",
		"tmp=$(mktemp) || exit 0",
		"marker=.crabbox/results-start",
		"git rev-parse --git-path 'crabbox/results-start'",
		"[ -f \"$marker\" ] || exit 0",
		"\"$marker\" -nt \"$f\"",
		"| sort > \"$tmp\"",
		"for want_failed in 1 0",
		"bs=4096 count=1",
		"grep -Eq '<testsuites?'",
		"grep -Eq '<(failure|error)([[:space:]>])'",
		"if [ \"$want_failed\" != \"$has_failed\" ]",
		"count=$((count + 1))",
		"bs=1048576 count=1",
		fmt.Sprintf(`if [ "$size" -gt %d ]`, autoJUnitMaxBytes),
		fmt.Sprintf(`if [ $((total + size)) -gt %d ]`, autoJUnitMaxTotalBytes),
		"cat \"$f\"",
		resultWarningMarker,
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

func TestWindowsRemoteFindJUnitResultFilesPrintsPathMarker(t *testing.T) {
	got := decodePowerShellCommand(t, windowsRemoteFindJUnitResultFiles(`C:\repo`, remoteResultsMarker))
	for _, want := range []string{
		"$ErrorActionPreference = \"Stop\"",
		"Set-Location -LiteralPath 'C:\\repo'",
		"$ErrorActionPreference = \"SilentlyContinue\"",
		"$marker = '.crabbox/results-start'",
		"git rev-parse --git-path 'crabbox/results-start'",
		"$marker = ([string]$gitMarker).Trim()",
		"function Get-CrabboxJUnitFiles",
		"$_.Name -ne 'node_modules' -and $_.Name -ne '.git'",
		"if (-not (Test-Path -LiteralPath $marker)) { return }",
		"$markerTime = (Get-Item -LiteralPath $marker).LastWriteTimeUtc",
		"$_.LastWriteTimeUtc -ge $markerTime",
		fmt.Sprintf("$maxBytes = %d", autoJUnitMaxBytes),
		fmt.Sprintf("$maxTotalBytes = %d", autoJUnitMaxTotalBytes),
		fmt.Sprintf("$failureSniffBytes = %d", autoJUnitFailureSniffBytes),
		"$sniffBytes = 4096",
		"$maxFiles = 50",
		"$prefix -notmatch '<testsuites?'",
		"$files = @(Get-CrabboxJUnitFiles (Get-Location).Path 5 | Sort-Object FullName)",
		"foreach ($wantFailed in @($true, $false))",
		"$hasFailed = $body -match '<(failure|error)(\\s|>)'",
		"$hasFailed -ne $wantFailed",
		"$count++",
		"$totalBytes += $fs.Length",
		resultWarningMarker,
		resultFileMarker + `$($file.FullName)`,
		"[System.IO.File]::OpenRead($file.FullName)",
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

func TestRemoteReadResultFilesConfinesResolvedPaths(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell regression")
	}
	root := t.TempDir()
	reports := filepath.Join(root, "reports")
	outside := t.TempDir()
	if err := os.Mkdir(reports, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(reports, "inside.xml"), "inside")
	write(filepath.Join(outside, "outside.xml"), "outside")
	links := map[string]string{
		filepath.Join(root, "safe-leaf.xml"):   filepath.Join(reports, "inside.xml"),
		filepath.Join(root, "safe-dir"):        reports,
		filepath.Join(root, "escape-leaf.xml"): filepath.Join(outside, "outside.xml"),
		filepath.Join(root, "escape-dir"):      outside,
	}
	for link, target := range links {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	fifo := filepath.Join(root, "result.fifo")
	if out, err := exec.Command("mkfifo", fifo).CombinedOutput(); err != nil {
		t.Fatalf("mkfifo: %v\n%s", err, out)
	}
	paths := []string{
		"reports/inside.xml",
		filepath.Join(reports, "inside.xml"),
		"safe-leaf.xml",
		"safe-dir/inside.xml",
		"escape-leaf.xml",
		"escape-dir/outside.xml",
		filepath.Join(outside, "outside.xml"),
		"result.fifo",
	}
	command := remoteReadResultFiles(root, paths)
	for _, want := range []string{`exec 3<"$candidate"`, `/proc/self/fd/3`, `echo $PPID > "$1"`, `lsof -a -p "$pid" -d 3`, `cat <&3`, `sleep 5; kill "$reader"`} {
		if !strings.Contains(command, want) {
			t.Fatalf("POSIX result reader missing descriptor-bound check %q:\n%s", want, command)
		}
	}
	started := time.Now()
	out, err := exec.Command("sh", "-c", command).CombinedOutput()
	if err != nil {
		t.Fatalf("remote result reader: %v\n%s", err, out)
	}
	if elapsed := time.Since(started); elapsed >= 5*time.Second {
		t.Fatalf("non-regular result path blocked collection for %s", elapsed)
	}
	files := parseMarkedFiles(string(out))
	for _, allowed := range paths[:4] {
		if files[allowed] != "inside" {
			t.Fatalf("allowed path %q missing: %#v", allowed, files)
		}
	}
	for _, escaped := range paths[4:] {
		if _, ok := files[escaped]; ok {
			t.Fatalf("escaped path %q was collected: %#v", escaped, files)
		}
	}
}

func TestWindowsRemoteReadResultFilesUsesResolvedConfinement(t *testing.T) {
	got := decodePowerShellCommand(t, windowsRemoteReadResultFiles(`C:\repo`, []string{`reports\junit.xml`}))
	for _, want := range []string{
		"GetFinalPathNameByHandle",
		"CreateFile",
		"$r=FinalPath $rh",
		"$s.SafeFileHandle",
		"$p.StartsWith($q,[StringComparison]::Ordinal)",
		"$z.ReadToEnd()",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("windows result reader missing %q:\n%s", want, got)
		}
	}
	command := windowsRemoteReadResultFiles(`C:\crabbox\cbx_123\crabbox`, []string{
		`.crabbox\junit-proof\inside.xml`,
		`C:\crabbox\cbx_123\crabbox\.crabbox\junit-proof\inside.xml`,
		`.crabbox\junit-proof\safe-leaf.xml`,
		`.crabbox\junit-proof\safe-dir\inside.xml`,
		`.crabbox\junit-proof\escape-leaf.xml`,
		`.crabbox\junit-proof\escape-dir\outside.xml`,
		`C:\Windows\Temp\crabbox-junit-proof\outside.xml`,
	})
	if len(command) >= 7500 {
		t.Fatalf("windows result reader exceeds cmd.exe command-line limit: %d bytes", len(command))
	}
}
