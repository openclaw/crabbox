package coder

import (
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestProviderClaimAmbiguity(t *testing.T) {
	claims := map[string]core.LeaseClaim{
		"first":  {LeaseID: "cbx_000000000001", Slug: "same-slug"},
		"second": {LeaseID: "cbx_000000000002", Slug: "same-slug"},
	}
	if c, ok, err := coderClaimForIdentifier(claims, "same-slug"); err == nil || !strings.Contains(err.Error(), "multiple") || ok || c.LeaseID != "" {
		t.Fatalf("ambiguous slug selected a claim: %+v %v %v", c, ok, err)
	}
	if c, ok, err := coderClaimForIdentifier(claims, "cbx_000000000001"); err != nil || !ok || c.LeaseID != "cbx_000000000001" {
		t.Fatalf("exact ID lost: %+v %v %v", c, ok, err)
	}
}
