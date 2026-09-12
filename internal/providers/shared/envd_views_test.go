package shared

import (
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestEnvdSandboxViewsPreserveMetadataAndStatusIdentity(t *testing.T) {
	views := EnvdSandboxViews{Provider: "compatible", LeasePrefix: "legacy_"}
	sandbox := EnvdSandbox{
		SandboxID:  "remote-id",
		State:      " RUNNING ",
		TemplateID: "template",
		Metadata: map[string]string{
			"provider": "untrusted", "target": "other", "lease": "  metadata-lease  ",
			"slug": "  display-slug  ", "state": "label-state", "custom": "kept",
		},
	}
	view := views.Status("requested-lease", sandbox)
	if view.ID != "requested-lease" || view.Labels["lease"] != "metadata-lease" {
		t.Fatalf("request and metadata identities conflated: %#v", view)
	}
	if view.Slug != "display-slug" || view.Labels["slug"] != "  display-slug  " {
		t.Fatalf("display and raw metadata slug conflated: %#v", view)
	}
	if view.State != " RUNNING " || view.Labels["state"] != "label-state" || !view.Ready {
		t.Fatalf("live and metadata states conflated: %#v", view)
	}
	if view.Provider != "compatible" || view.Labels["provider"] != "compatible" || view.Labels["target"] != core.TargetLinux || view.Labels["custom"] != "kept" {
		t.Fatalf("metadata projection = %#v", view)
	}
	view.Labels["custom"] = "changed"
	if sandbox.Metadata["custom"] != "kept" || sandbox.Metadata["provider"] != "untrusted" {
		t.Fatal("view mutated source metadata")
	}
}

func TestEnvdSandboxViewsUseAdapterFallbackIdentity(t *testing.T) {
	for _, prefix := range []string{"e2b_", "cubesandbox_"} {
		views := EnvdSandboxViews{Provider: "compatible", LeasePrefix: prefix}
		sandbox := EnvdSandbox{SandboxID: "remote-id"}
		server := views.Server(sandbox)
		leaseID := prefix + "remote-id"
		if server.Labels["lease"] != leaseID || server.Labels["slug"] != core.NewLeaseSlug(leaseID) || server.ServerType.Name != "base" {
			t.Fatalf("fallback projection = %#v", server)
		}
		if _, exists := server.Labels["state"]; !exists {
			t.Fatal("empty state label was omitted")
		}
		if !views.Status(leaseID, sandbox).Ready {
			t.Fatal("compatible empty state stopped being ready")
		}
	}
}
