package shared

import (
	"strings"
	"testing"
)

func TestSandboxNameBase(t *testing.T) {
	for _, tt := range []struct {
		name, repo, prefix string
		maxBase            int
		want               string
	}{
		{name: "normalized", repo: " My_App / TEST! ", prefix: "crabbox-", maxBase: 48, want: "my-app-test"},
		{name: "non ASCII", repo: "café APP", prefix: "crabbox-", maxBase: 48, want: "caf-app"},
		{name: "empty", prefix: "crabbox-", maxBase: 48, want: "crabbox"},
		{name: "punctuation", repo: " _/! ", prefix: "crabbox-", maxBase: 48, want: "crabbox"},
		{name: "redundant prefix", repo: "CRABBOX-App", prefix: "crabbox-", maxBase: 48, want: "app"},
		{name: "strip once", repo: "crabbox-crabbox-app", prefix: "crabbox-", maxBase: 48, want: "crabbox-app"},
		{name: "prefix word", repo: "crabbox", prefix: "crabbox-", maxBase: 48, want: "crabbox"},
		{name: "prefix without dash", repo: "crabbox-app", prefix: "crabbox", maxBase: 48, want: "app"},
		{name: "exact 48", repo: strings.Repeat("a", 48), prefix: "crabbox-", maxBase: 48, want: strings.Repeat("a", 48)},
		{name: "over 48", repo: strings.Repeat("a", 48) + "b", prefix: "crabbox-", maxBase: 48, want: strings.Repeat("a", 48)},
		{name: "exact 34", repo: strings.Repeat("b", 34), prefix: "crabbox-", maxBase: 34, want: strings.Repeat("b", 34)},
		{name: "over 34", repo: strings.Repeat("b", 34) + "c", prefix: "crabbox-", maxBase: 34, want: strings.Repeat("b", 34)},
		{name: "truncate hyphen", repo: "abc-def", prefix: "crabbox-", maxBase: 4, want: "abc"},
		{name: "zero budget", repo: "app", prefix: "crabbox-", maxBase: 0, want: "a"},
		{name: "negative budget", repo: "app", prefix: "crabbox-", maxBase: -1, want: "a"},
		{name: "fallback before truncation", prefix: "crabbox-", maxBase: 1, want: "c"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := SandboxNameBase(tt.repo, tt.prefix, tt.maxBase); got != tt.want {
				t.Fatalf("SandboxNameBase(%q, %q, %d) = %q, want %q", tt.repo, tt.prefix, tt.maxBase, got, tt.want)
			}
		})
	}
}
