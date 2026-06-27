package vast

import (
	"strings"
	"testing"
)

func TestOwnershipLabelRoundTrip(t *testing.T) {
	label := encodeVastOwnershipLabel("lease_123", "gpu-box", "active")
	got, ok := decodeVastOwnershipLabel(label)
	if !ok {
		t.Fatalf("label did not decode: %q", label)
	}
	if got.LeaseID != "lease_123" || got.Slug != "gpu-box" || got.State != "active" {
		t.Fatalf("decoded=%#v", got)
	}
	if !isVastCrabboxOwnedLabel(label) {
		t.Fatalf("owned label not recognized: %q", label)
	}
}

func TestOwnershipLabelRejectsMalformedAndManualLabels(t *testing.T) {
	for _, label := range []string{
		"",
		"crabbox lease lease_123",
		"manual-crabbox-instance",
		"cbx1|",
		"cbx1|lease|missing-state",
		"cbx2|lease|slug|active",
	} {
		if _, ok := decodeVastOwnershipLabel(label); ok {
			t.Fatalf("label %q decoded as owned", label)
		}
		if isVastCrabboxOwnedLabel(label) {
			t.Fatalf("label %q recognized as owned", label)
		}
	}
}

func TestOwnershipLabelSanitizesAndBoundsParts(t *testing.T) {
	label := encodeVastOwnershipLabel("lease/with spaces", strings.Repeat("x", 80), "active now")
	if strings.ContainsAny(label, " /") {
		t.Fatalf("label was not sanitized: %q", label)
	}
	got, ok := decodeVastOwnershipLabel(label)
	if !ok {
		t.Fatalf("label did not decode: %q", label)
	}
	if got.LeaseID != "lease-with-spaces" || len(got.Slug) != vastLabelMaxPart || got.State != "active-now" {
		t.Fatalf("decoded=%#v label=%q", got, label)
	}
}
