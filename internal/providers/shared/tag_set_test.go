package shared

import (
	"reflect"
	"slices"
	"testing"
)

func TestNormalizeTags(t *testing.T) {
	for _, tt := range []struct {
		name string
		tags []string
		want []string
	}{
		{"nil", nil, []string{}},
		{"empty", []string{}, []string{}},
		{"blank", []string{"", " \t\n", "\u2003"}, []string{}},
		{"set", []string{" z ", "a", "z", " A", "\na\t", "a:b", "a:b"}, []string{"A", "a", "a:b", "z"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			original := slices.Clone(tt.tags)
			got := NormalizeTags(tt.tags)
			if !reflect.DeepEqual(got, tt.want) || !reflect.DeepEqual(tt.tags, original) {
				t.Fatalf("tags=%q result=%q, want unchanged input and %q", tt.tags, got, tt.want)
			}
			if len(got) > 0 {
				got[0] = "changed"
				if !reflect.DeepEqual(tt.tags, original) {
					t.Fatal("normalized tags alias the caller's slice")
				}
			}
		})
	}
}
