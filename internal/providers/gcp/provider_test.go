package gcp

import (
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestPrepareLeaseClaimEndpointPreservesExactGCPIdentity(t *testing.T) {
	existing := core.LeaseClaim{
		LeaseID:        "cbx_123456789abc",
		CloudID:        "crabbox-owned-cbx_123456789abc",
		CloudNumericID: 42,
		Slug:           "owned",
		Labels: map[string]string{
			"lease":        "cbx_123456789abc",
			"slug":         "owned",
			"zone":         "us-central1-b",
			"provider_key": "crabbox-owner",
		},
	}
	server := core.Server{
		Provider: "gcp",
		CloudID:  existing.CloudID,
		ID:       existing.CloudNumericID,
		Labels: map[string]string{
			"provider":     "gcp",
			"lease":        existing.LeaseID,
			"slug":         existing.Slug,
			"zone":         "us-central1-b",
			"provider_key": "crabbox-owner",
			"state":        "ready",
		},
	}

	prepared, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "gcp", existing.Slug, server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ID != existing.CloudNumericID || prepared.Labels["zone"] != "us-central1-b" || prepared.Labels["provider_key"] != "crabbox-owner" {
		t.Fatalf("prepared=%+v, want preserved exact identity", prepared)
	}

	for name, mutate := range map[string]func(*core.Server){
		"name":         func(server *core.Server) { server.CloudID += "-other" },
		"numeric id":   func(server *core.Server) { server.ID++ },
		"zone":         func(server *core.Server) { server.Labels["zone"] = "us-central1-c" },
		"provider key": func(server *core.Server) { server.Labels["provider_key"] = "crabbox-other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := server
			changed.Labels = cloneTestLabels(server.Labels)
			mutate(&changed)
			if _, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "gcp", existing.Slug, changed, false); err == nil || !strings.Contains(err.Error(), "refusing to rewrite GCP") {
				t.Fatalf("error=%v, want exact-identity refusal", err)
			}
		})
	}
}

func TestPrepareLeaseClaimEndpointDoesNotPromoteLegacyGCPIdentity(t *testing.T) {
	existing := core.LeaseClaim{
		LeaseID: "cbx_legacy123456",
		CloudID: "crabbox-legacy-cbx_legacy123456",
		Slug:    "legacy",
		Labels:  map[string]string{"lease": "cbx_legacy123456", "slug": "legacy"},
	}
	server := core.Server{
		Provider: "gcp",
		CloudID:  existing.CloudID,
		ID:       99,
		Labels: map[string]string{
			"provider": "gcp", "lease": existing.LeaseID, "slug": existing.Slug,
			"zone": "us-central1-b", "provider_key": "crabbox-cloud",
		},
	}
	prepared, err := (Provider{}).PrepareLeaseClaimEndpoint(existing, "gcp", existing.Slug, server, false)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ID != 0 || prepared.Labels["zone"] != "" || prepared.Labels["provider_key"] != "" {
		t.Fatalf("prepared=%+v, must not promote mutable cloud identity into a legacy claim", prepared)
	}
}

func cloneTestLabels(labels map[string]string) map[string]string {
	cloned := make(map[string]string, len(labels))
	for key, value := range labels {
		cloned[key] = value
	}
	return cloned
}
