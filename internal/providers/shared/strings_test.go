package shared

import "testing"

func TestFirstStringWhitespacePolicies(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		values                []string
		empty, blank, trimmed string
	}{
		{name: "none"},
		{name: "empty", values: []string{"", ""}},
		{name: "blank", values: []string{"", " \t\n"}, empty: " \t\n"},
		{name: "skip blanks", values: []string{"", " \t", " value ", "later"}, empty: " \t", blank: " value ", trimmed: "value"},
		{name: "preserve first", values: []string{" first ", "second"}, empty: " first ", blank: " first ", trimmed: "first"},
		{name: "unicode whitespace", values: []string{"\u2003", "\u00a0value\u00a0", "later"}, empty: "\u2003", blank: "\u00a0value\u00a0", trimmed: "value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := FirstNonEmpty(tc.values...); got != tc.empty {
				t.Errorf("FirstNonEmpty = %q, want %q", got, tc.empty)
			}
			if got := FirstNonBlank(tc.values...); got != tc.blank {
				t.Errorf("FirstNonBlank = %q, want %q", got, tc.blank)
			}
			if got := FirstNonBlankTrimmed(tc.values...); got != tc.trimmed {
				t.Errorf("FirstNonBlankTrimmed = %q, want %q", got, tc.trimmed)
			}
		})
	}
}
