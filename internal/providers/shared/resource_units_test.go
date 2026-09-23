package shared

import (
	"math"
	"testing"
)

func TestMiBToBytes(t *testing.T) {
	const maxMiB = math.MaxInt64 / (1 << 20)
	for _, tc := range []struct {
		name       string
		mib, bytes int64
		valid      bool
	}{
		{"negative", -1, 0, false},
		{"zero", 0, 0, true},
		{"normal", 8192, 8589934592, true},
		{"maximum", maxMiB, math.MaxInt64 - (1 << 20) + 1, true},
		{"over maximum", maxMiB + 1, 0, false},
		{"wrap to positive", 17592186052608, 0, false},
		{"maximum int64", math.MaxInt64, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := MiBToBytes(tc.mib)
			if got != tc.bytes || ok != tc.valid {
				t.Fatalf("MiBToBytes(%d) = %d, %t; want %d, %t", tc.mib, got, ok, tc.bytes, tc.valid)
			}
		})
	}
}
