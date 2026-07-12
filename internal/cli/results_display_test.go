package cli

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestHumanResultsEscapeTerminalControlsWithoutChangingJSON(t *testing.T) {
	failure := TestFailure{
		Suite:     "suite\nnext",
		Name:      "case\x1b[2J",
		Classname: "pkg\u202e",
		File:      "test\tfile\u2066",
		Message:   "bad\rrewrite\x1b]52;c;payload\x07\nstack",
		Type:      "AssertionError\x1b[31m",
		Kind:      "failure\u0085",
	}
	summary := TestResultSummary{Format: "junit", Tests: 1, Failures: 1, Failed: []TestFailure{failure}}

	for _, test := range []struct {
		name  string
		print func(*bytes.Buffer)
	}{
		{name: "summary", print: func(out *bytes.Buffer) { printTestResults(out, summary) }},
		{name: "failed only", print: func(out *bytes.Buffer) { printTestFailuresOnly(out, summary) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			test.print(&out)
			text := out.String()
			for _, want := range []string{
				`test\u0009file\u2066`,
				`failure\u0085`,
				`pkg\u202E.case\u001B[2J`,
				`bad\u000Drewrite\u001B]52;c;payload\u0007`,
			} {
				if !strings.Contains(text, want) {
					t.Fatalf("human output missing %q: %q", want, text)
				}
			}
			assertTerminalSafe(t, text)
		})
	}

	encoded, err := json.Marshal(summary)
	if err != nil {
		t.Fatal(err)
	}
	var decoded TestResultSummary
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, summary) {
		t.Fatalf("JSON result changed\n got: %#v\nwant: %#v", decoded, summary)
	}
}

func TestTerminalSafeResultFieldEscapesC0C1AndUnicodeFormatting(t *testing.T) {
	got := terminalSafeResultField("a\x00\x1b\x7f\u0085\u2028\u2029\u202e\U000E0001z")
	want := `a\u0000\u001B\u007F\u0085\u2028\u2029\u202E\U000E0001z`
	if got != want {
		t.Fatalf("terminalSafeResultField=%q want %q", got, want)
	}
}

func assertTerminalSafe(t *testing.T, value string) {
	t.Helper()
	for _, r := range value {
		if isTerminalControl(r) && r != '\n' {
			t.Fatalf("output contains terminal control U+%04X: %q", r, value)
		}
	}
}
