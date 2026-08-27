package runnerfs

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestExplicitPOSIXBackslashIsNotASeparator(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("literal POSIX filename")
	}
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, `reports\junit.xml`), "<testsuite tests=\"1\"/>")
	writeFixture(t, filepath.Join(dir, "reports", "junit.xml"), "<testsuite tests=\"1\" failures=\"1\"/>")
	result, err := root.CollectResults(t.Context(), ResultOptions{Paths: []string{`reports\junit.xml`, "reports/junit.xml"}, ExplicitMaxBytes: 1024, ExplicitTotalBytes: 2048})
	if err != nil || len(result.Files) != 2 {
		t.Fatalf("files=%v err=%v", result.Files, err)
	}
}

func TestReportSymlinkAliasesDoNotDuplicateOrConsumeBudget(t *testing.T) {
	root, dir := testRoot(t)
	const content = "<testsuite/>"
	writeFixture(t, filepath.Join(dir, "junit.xml"), content)
	writeFixture(t, filepath.Join(dir, "junit-other.xml"), content)
	symlinkFixture(t, "junit.xml", filepath.Join(dir, "alias.xml"))
	result, err := root.CollectResults(t.Context(), ResultOptions{
		Paths: []string{"alias.xml", "junit.xml"}, Auto: true,
		ExplicitMaxBytes: int64(len(content)), ExplicitTotalBytes: int64(len(content)),
	})
	if err != nil || len(result.Files) != 2 || result.Files[0].Path != "alias.xml" || result.Files[1].Path != "junit-other.xml" || len(result.Warnings) != 0 {
		t.Fatalf("files=%v warnings=%v err=%v", result.Files, result.Warnings, err)
	}
}

func TestJUnitFilenamePreservesPlatformCaseRules(t *testing.T) {
	for _, name := range []string{"JUNIT.XML", "test-one.XML", "RESULTS.XML"} {
		if !junitFilenameForOS("windows", name) || junitFilenameForOS("linux", name) {
			t.Fatalf("unexpected platform matching for %q", name)
		}
	}
}

func TestCollectResultsFreshnessPruningAndDeduplication(t *testing.T) {
	root, dir := testRoot(t)
	const good = `<testsuite tests="1"><testcase name="ok"/></testsuite>`
	for _, name := range []string{"reports/junit-ok.xml", "reports/junit-old.xml", ".git/junit-hidden.xml", "node_modules/junit-hidden.xml", "nested/node_modules/TEST-hidden.xml", "plain.xml"} {
		writeFixture(t, filepath.Join(dir, filepath.FromSlash(name)), good)
	}
	after := time.Now().Add(-time.Minute)
	old := after.Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "reports/junit-old.xml"), old, old); err != nil {
		t.Fatal(err)
	}
	options := ResultOptions{Paths: []string{"reports/junit-ok.xml", filepath.Join(dir, "reports/junit-ok.xml"), "plain.xml"}, Auto: true, After: after, ExplicitMaxBytes: 1024, ExplicitTotalBytes: 4096}
	result, err := root.CollectResults(context.Background(), options)
	if err != nil || len(result.Files) != 2 || len(result.Warnings) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if result.Files[0].Path != "reports/junit-ok.xml" || result.Files[1].Path != "plain.xml" {
		t.Fatalf("paths changed: %+v", result.Files)
	}
}

func TestAutoResultsPrioritizeLateFailuresWithinCountLimit(t *testing.T) {
	root, dir := testRoot(t)
	for i := 0; i < 65; i++ {
		writeFixture(t, filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", i)), `<testsuite tests="1"/>`)
	}
	writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), `<testsuite tests="1"><testcase><failure message="expected"/></testcase></testsuite>`)
	result, err := root.CollectResults(context.Background(), ResultOptions{Auto: true})
	if err != nil || len(result.Files) != AutoMaxFiles || result.Files[0].Path != "junit-zzz.xml" {
		t.Fatalf("result count=%d err=%v", len(result.Files), err)
	}
}

func TestAutoResultsPrioritizeSelfClosingFailures(t *testing.T) {
	for _, element := range []string{"<failure/>", "<error/>"} {
		t.Run(element, func(t *testing.T) {
			root, dir := testRoot(t)
			for index := range AutoMaxFiles {
				writeFixture(t, filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", index)), "<testsuite/>")
			}
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), "<testsuite><testcase>"+element+"</testcase></testsuite>")
			result, err := root.CollectResults(t.Context(), ResultOptions{Auto: true})
			if err != nil || len(result.Files) != AutoMaxFiles || result.Files[0].Path != "junit-zzz.xml" {
				t.Fatalf("late failure omitted: files=%d err=%v", len(result.Files), err)
			}
		})
	}
}

func TestAutoResultsIgnoreFailureTextOutsideElements(t *testing.T) {
	for _, content := range []string{
		`<!-- <failure/> -->`,
		`<!-- <error/> -->`,
		`<system-out><![CDATA[<failure/>]]></system-out>`,
		`<system-out><![CDATA[<error/>]]></system-out>`,
		`<system-out>&lt;failure/&gt;</system-out>`,
		`<failure-count>0</failure-count>`,
	} {
		t.Run(content, func(t *testing.T) {
			root, dir := testRoot(t)
			for index := range AutoMaxFiles + 2 {
				writeFixture(t, filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", index)), `<testsuite tests="1" failures="0">`+content+`</testsuite>`)
			}
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), `<testsuite><testcase><failure/></testcase></testsuite>`)
			result, err := root.CollectResults(t.Context(), ResultOptions{Auto: true})
			if err != nil || len(result.Files) != AutoMaxFiles || result.Files[0].Path != "junit-zzz.xml" {
				t.Fatalf("literal failure text displaced the failing report: files=%d err=%v", len(result.Files), err)
			}
		})
	}
}

func TestAutoResultsPriorityIgnoresTextEncodingDeclarations(t *testing.T) {
	for _, encoding := range []string{"US-ASCII", "ISO-8859-1", "windows-1252"} {
		t.Run(encoding, func(t *testing.T) {
			root, dir := testRoot(t)
			for index := range AutoMaxFiles {
				writeFixture(t, filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", index)), "<testsuite/>")
			}
			output := "plain text"
			if encoding != "US-ASCII" {
				output = "encoded caf\xe9"
			}
			report := `<?xml version="1.0" encoding="` + encoding + `"?><testsuite><system-out>` + output + `</system-out><testcase><failure/></testcase></testsuite>`
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), report)
			result, err := root.CollectResults(t.Context(), ResultOptions{Auto: true})
			if err != nil || len(result.Files) != AutoMaxFiles || result.Files[0].Path != "junit-zzz.xml" || string(result.Files[0].Data) != report {
				t.Fatalf("encoding affected discovery priority or report bytes: files=%d err=%v", len(result.Files), err)
			}
		})
	}
}

func TestAutoResultsPrioritizeOpeningTagCrossingSniffLimit(t *testing.T) {
	for _, element := range []string{"failure", "error"} {
		t.Run(element, func(t *testing.T) {
			root, dir := testRoot(t)
			for index := range AutoMaxFiles {
				writeFixture(t, filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", index)), "<testsuite/>")
			}
			report := `<testsuite><testcase><` + element + ` message="` + strings.Repeat("x", AutoFailureBytes) + `"/></testcase></testsuite>`
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), report)
			result, err := root.CollectResults(t.Context(), ResultOptions{Auto: true})
			if err != nil || len(result.Files) != AutoMaxFiles || result.Files[0].Path != "junit-zzz.xml" {
				t.Fatalf("partial opening tag displaced the failing report: files=%d err=%v", len(result.Files), err)
			}
		})
	}
}

func TestAutoResultsDeduplicateBeforeCandidateLimit(t *testing.T) {
	for _, explicit := range []bool{false, true} {
		t.Run(fmt.Sprintf("explicit=%v", explicit), func(t *testing.T) {
			root, dir := testRoot(t)
			options := ResultOptions{Auto: true, ExplicitMaxBytes: 64, ExplicitTotalBytes: 4096}
			first := filepath.Join(dir, "junit-000.xml")
			for index := range AutoMaxFiles + 1 {
				name := fmt.Sprintf("junit-%03d.xml", index)
				if explicit || index == 0 {
					writeFixture(t, filepath.Join(dir, name), "<testsuite/>")
				} else if err := os.Link(first, filepath.Join(dir, name)); err != nil {
					t.Skipf("hard-link fixture unavailable: %v", err)
				}
				if explicit {
					options.Paths = append(options.Paths, name)
				}
			}
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), "<testsuite/>")
			result, err := root.CollectResults(t.Context(), options)
			want := 2
			if explicit {
				want = len(options.Paths) + 1
			}
			if err != nil || len(result.Files) != want || result.Files[len(result.Files)-1].Path != "junit-zzz.xml" {
				t.Fatalf("duplicates exhausted candidates: files=%d want=%d err=%v", len(result.Files), want, err)
			}
		})
	}
}

func TestAutoResultsOversizedFilesDoNotConsumeCandidateSlots(t *testing.T) {
	for _, body := range []string{"<testsuite/>", "<testsuite><failure/></testsuite>"} {
		t.Run(body, func(t *testing.T) {
			root, dir := testRoot(t)
			for index := range AutoMaxFiles {
				name := filepath.Join(dir, fmt.Sprintf("junit-%03d.xml", index))
				writeFixture(t, name, body)
				if err := os.Truncate(name, AutoMaxFileBytes+1); err != nil {
					t.Fatal(err)
				}
			}
			writeFixture(t, filepath.Join(dir, "junit-zzz.xml"), body)
			result, err := root.CollectResults(t.Context(), ResultOptions{Auto: true})
			if err != nil || len(result.Files) != 1 || result.Files[0].Path != "junit-zzz.xml" || len(result.Warnings) != AutoMaxFiles {
				t.Fatalf("files=%d warnings=%d err=%v", len(result.Files), len(result.Warnings), err)
			}
		})
	}
}

func TestAutoResultsNeverFollowSymlinkCandidates(t *testing.T) {
	root, dir := testRoot(t)
	outside := filepath.Join(t.TempDir(), "outside.xml")
	writeFixture(t, outside, `<testsuite tests="900"/>`)
	symlinkFixture(t, outside, filepath.Join(dir, "junit-outside.xml"))
	result, err := root.CollectResults(context.Background(), ResultOptions{Auto: true})
	if err != nil || len(result.Files) != 0 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAutoResultsWarnInsteadOfTruncatingOversizedReport(t *testing.T) {
	root, dir := testRoot(t)
	name := filepath.Join(dir, "junit-big.xml")
	f, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(`<testsuite tests="1">`); err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(AutoMaxFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := root.CollectResults(context.Background(), ResultOptions{Auto: true})
	if err != nil || len(result.Files) != 0 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0].Message, "per-file limit") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestCanceledResultCollectionReturnsCancellation(t *testing.T) {
	root, _ := testRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := root.CollectResults(ctx, ResultOptions{Auto: true}); err != context.Canceled {
		t.Fatalf("err=%v", err)
	}
}

func TestCanceledEmptyResultCollectionReturnsCancellation(t *testing.T) {
	root, _ := testRoot(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := root.CollectResults(ctx, ResultOptions{}); err != context.Canceled {
		t.Fatalf("empty canceled collection: %v", err)
	}
}

func TestExplicitLimitDoesNotChargeDuplicatePaths(t *testing.T) {
	root, dir := testRoot(t)
	writeFixture(t, filepath.Join(dir, "one.xml"), "1234")
	writeFixture(t, filepath.Join(dir, "two.xml"), "5678")
	result, err := root.CollectResults(context.Background(), ResultOptions{
		Paths:            []string{"one.xml", "./one.xml", filepath.Join(dir, "one.xml"), "two.xml"},
		ExplicitMaxBytes: 4, ExplicitTotalBytes: 4,
	})
	if err != nil || len(result.Files) != 1 || len(result.Warnings) != 1 || result.Warnings[0].Path != "two.xml" {
		t.Fatalf("files=%d warnings=%+v err=%v", len(result.Files), result.Warnings, err)
	}
}

func TestAutoAggregateLimitNeverReturnsPartialReport(t *testing.T) {
	root, dir := testRoot(t)
	prefix, suffix := []byte(`<testsuite tests="0"><!--`), []byte(`--></testsuite>`)
	padding := bytes.Repeat([]byte{' '}, 64<<10)
	for i := 0; i < 5; i++ {
		f, err := os.Create(filepath.Join(dir, fmt.Sprintf("junit-%d.xml", i)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Write(prefix); err != nil {
			t.Fatal(err)
		}
		remaining := AutoMaxFileBytes - len(prefix) - len(suffix)
		for remaining > 0 {
			count := min(remaining, len(padding))
			if _, err = f.Write(padding[:count]); err != nil {
				t.Fatal(err)
			}
			remaining -= count
		}
		if _, err = f.Write(suffix); err != nil {
			t.Fatal(err)
		}
		if err = f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	result, err := root.CollectResults(context.Background(), ResultOptions{Auto: true})
	if err != nil || len(result.Files) != 4 || len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0].Message, "aggregate limit") {
		t.Fatalf("files=%d warnings=%+v err=%v", len(result.Files), result.Warnings, err)
	}
	for _, file := range result.Files {
		if len(file.Data) != AutoMaxFileBytes || !bytes.HasSuffix(file.Data, suffix) {
			t.Fatalf("truncated %s", file.Path)
		}
	}
}

func TestCandidateRetentionIsBoundedAndLexicallySorted(t *testing.T) {
	var values []resultCandidate
	for i := 999; i >= 0; i-- {
		values = retainResultCandidate(values, resultCandidate{name: fmt.Sprintf("junit-%04d.xml", i)})
		if len(values) > AutoMaxFiles {
			t.Fatal("unbounded candidate list")
		}
	}
	for i, value := range values {
		if want := fmt.Sprintf("junit-%04d.xml", i); value.name != want {
			t.Fatalf("index %d=%s want=%s", i, value.name, want)
		}
	}
}
