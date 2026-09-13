package shared

import "testing"

func TestLowercaseHostnamePreservesZoneIdentity(t *testing.T) {
	for _, tt := range []struct{ input, want string }{
		{"EXAMPLE.COM", "example.com"},
		{"EXAMPLE.COM.", "example.com."},
		{" EXAMPLE.COM ", " example.com "},
		{"FE80::ABCD%enABC", "fe80::abcd%enABC"},
		{"FE80:0:0::ABCD%EN0", "fe80:0:0::abcd%EN0"},
		{"FE80::ABCD", "fe80::abcd"},
		{"NAME%SUFFIX", "name%suffix"},
		{"", ""},
	} {
		if got := LowercaseHostname(tt.input); got != tt.want {
			t.Errorf("LowercaseHostname(%q)=%q, want %q", tt.input, got, tt.want)
		}
	}
}
