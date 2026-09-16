package gcp

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestReadyPoolImageScopeGrammar(t *testing.T) {
	for _, tc := range []struct {
		sourceID string
		kind     string
		scope    string
	}{
		{"projects/source-project/global/images/runner-v3", "gcp-image", "projects/source-project/global/images"},
		{"https://compute.googleapis.com/compute/v1/projects/source-project/global/images/runner-v3", "gcp-image", "projects/source-project/global/images"},
		{"https://www.googleapis.com/compute/v1/projects/source-project/global/snapshots/runner-v3", "gcp-disk-snapshot", "projects/source-project/global/snapshots"},
	} {
		scope, ok := readyPoolImageScope(tc.sourceID, tc.kind)
		if !ok || scope != tc.scope {
			t.Fatalf("source=%q kind=%q scope=%q ok=%t want=%q", tc.sourceID, tc.kind, scope, ok, tc.scope)
		}
	}
	for _, tc := range []struct {
		sourceID string
		kind     string
	}{
		{" projects/p/global/images/i", "gcp-image"},
		{"projects/p/global/images/i ", "gcp-image"},
		{"https://compute.googleapis.com:443/compute/v1/projects/p/global/images/i", "gcp-image"},
		{"https://compute.googleapis.com/compute/v1/projects/p/global/images/i?x=1", "gcp-image"},
		{"https://compute.googleapis.com/compute/v1/projects/p/global/images/i#x", "gcp-image"},
		{"projects/p/global/images/i%2fextra", "gcp-image"},
		{"projects/p/global/images/family/i", "gcp-image"},
		{"projects/p/global/images/i/extra", "gcp-image"},
		{"projects/p/global/snapshots/i", "gcp-image"},
		{"projects/p/global/images/i", "gcp-machine-image"},
		{"Projects/p/global/images/i", "gcp-image"},
	} {
		if scope, ok := readyPoolImageScope(tc.sourceID, tc.kind); ok {
			t.Fatalf("invalid source=%q kind=%q accepted as %q", tc.sourceID, tc.kind, scope)
		}
	}
}

func TestReadyPoolImageIdentityMatchesLease(t *testing.T) {
	request := core.ProviderReadyPoolImageIdentityRequest{
		Identity: core.CoordinatorReadyPoolImageIdentity{
			Provider: "gcp", Scope: "projects/source-project/global/images", ID: "1234567890123456789",
		},
		Lease: core.ProviderReadyPoolLeaseImageIdentity{
			Provider: "gcp", Project: "execution-project", Region: "europe-west4-a",
			Image: &core.CoordinatorLeaseImage{
				Provider: "gcp", Kind: "gcp-image", ID: "1234567890123456789",
				SourceID: "https://www.googleapis.com/compute/v1/projects/source-project/global/images/runner-v3",
			},
		},
	}
	if !(Provider{}).ReadyPoolImageIdentityMatchesLease(request) {
		t.Fatal("valid GCP image identity rejected")
	}
	for _, tc := range []struct {
		name   string
		mutate func(*core.ProviderReadyPoolImageIdentityRequest)
	}{
		{"identity provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Identity.Provider = "aws" }},
		{"lease provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Provider = "aws" }},
		{"missing image", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image = nil }},
		{"image provider", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.Provider = "aws" }},
		{"missing execution project", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Project = "" }},
		{"noncanonical execution project", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Project = " project " }},
		{"source project", func(req *core.ProviderReadyPoolImageIdentityRequest) {
			req.Lease.Image.SourceID = "projects/other-project/global/images/runner-v3"
		}},
		{"source collection", func(req *core.ProviderReadyPoolImageIdentityRequest) {
			req.Lease.Image.Kind = "gcp-disk-snapshot"
			req.Lease.Image.SourceID = "projects/source-project/global/snapshots/runner-v3"
		}},
		{"invalid identity scope", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Identity.Scope = "source-project" }},
		{"nonnumeric identity id", func(req *core.ProviderReadyPoolImageIdentityRequest) {
			req.Identity.ID = "image-id"
			req.Lease.Image.ID = "image-id"
		}},
		{"image id", func(req *core.ProviderReadyPoolImageIdentityRequest) { req.Lease.Image.ID = "987654321" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := request
			image := *request.Lease.Image
			changed.Lease.Image = &image
			tc.mutate(&changed)
			if (Provider{}).ReadyPoolImageIdentityMatchesLease(changed) {
				t.Fatalf("mismatched request accepted: %#v", changed)
			}
		})
	}
}

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

func TestGCPConfigShowCompletePassiveSection(t *testing.T) {
	projector, ok := any(Provider{}).(core.ProviderConfigShowProjector)
	if !ok {
		t.Fatal("actual provider has no passive config-show projector")
	}
	for _, tc := range []struct {
		name  string
		input core.Config
		want  map[string]any
		text  string
	}{
		{name: "nil", input: core.Config{}, want: map[string]any{"project": "", "zone": "", "image": "", "network": "", "subnet": "", "tags": []string(nil), "rootGB": int64(0), "sshCIDRs": []string(nil), "serviceAccount": ""}, text: "gcp project=- zone= image= network= subnet=- root_gb=0 ssh_cidrs=-\n"},
		{name: "empty", input: core.Config{GCPTags: []string{}, GCPSSHCIDRs: []string{}}, want: map[string]any{"project": "", "zone": "", "image": "", "network": "", "subnet": "", "tags": []string{}, "rootGB": int64(0), "sshCIDRs": []string{}, "serviceAccount": ""}, text: "gcp project=- zone= image= network= subnet=- root_gb=0 ssh_cidrs=-\n"},
		{name: "raw-references-list", input: core.Config{GCPProject: "project-reference", GCPZone: "raw-zone", GCPImage: "image-reference", GCPNetwork: "network-reference", GCPSubnet: "subnet-reference", GCPTags: []string{"last", "first", "last", " "}, GCPRootGB: 9007199254740993, GCPSSHCIDRs: []string{"second", "first", "second", " "}, GCPServiceAccount: "identity-reference"}, want: map[string]any{"project": "project-reference", "zone": "raw-zone", "image": "image-reference", "network": "network-reference", "subnet": "subnet-reference", "tags": []string{"last", "first", "last", " "}, "rootGB": int64(9007199254740993), "sshCIDRs": []string{"second", "first", "second", " "}, "serviceAccount": "identity-reference"}, text: "gcp project=project-reference zone=raw-zone image=image-reference network=network-reference subnet=subnet-reference root_gb=9007199254740993 ssh_cidrs=second,first,second, \n"},
		{name: "whitespace-empty-elements", input: core.Config{GCPProject: " ", GCPZone: " ", GCPImage: " ", GCPNetwork: " ", GCPSubnet: " ", GCPTags: []string{"", ""}, GCPRootGB: -1, GCPSSHCIDRs: []string{"", ""}, GCPServiceAccount: " "}, want: map[string]any{"project": " ", "zone": " ", "image": " ", "network": " ", "subnet": " ", "tags": []string{"", ""}, "rootGB": int64(-1), "sshCIDRs": []string{"", ""}, "serviceAccount": " "}, text: "gcp project=  zone=  image=  network=  subnet=  root_gb=-1 ssh_cidrs=,\n"},
	} {
		for _, selected := range []string{"gcp", "static"} {
			t.Run(tc.name+"/"+selected, func(t *testing.T) {
				cfg := tc.input
				cfg.Provider = selected
				before := cfg
				before.GCPTags = slices.Clone(cfg.GCPTags)
				before.GCPSSHCIDRs = slices.Clone(cfg.GCPSSHCIDRs)
				section := projector.ConfigShowSection(cfg)
				if section.JSONKey != "gcp" || section.TextLabel != "gcp" || !reflect.DeepEqual(section.Providers, []string{"gcp"}) {
					t.Fatalf("section metadata=%#v", section)
				}
				wantOrder := []string{"project", "zone", "image", "network", "subnet", "tags", "rootGB", "sshCIDRs", "serviceAccount"}
				if len(section.Fields) != len(wantOrder) {
					t.Fatalf("field count=%d want %d", len(section.Fields), len(wantOrder))
				}
				got := map[string]any{}
				line := section.TextLabel
				for i, field := range section.Fields {
					if field.JSONName != wantOrder[i] {
						t.Fatalf("field %d name=%q want %q", i, field.JSONName, wantOrder[i])
					}
					got[field.JSONName] = field.JSONValue
					if field.TextName != "" {
						line += " " + field.TextName + "=" + field.TextValue
					}
				}
				line += "\n"
				if !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("public fields=%#v want %#v", got, tc.want)
				}
				if line != tc.text {
					t.Fatalf("text=%q want %q", line, tc.text)
				}
				if !reflect.DeepEqual(cfg, before) {
					t.Fatal("projection mutated supplied configuration")
				}
			})
		}
	}
}
