package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	gcpcompute "cloud.google.com/go/compute/apiv1"
	"cloud.google.com/go/compute/apiv1/computepb"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
	"google.golang.org/protobuf/proto"
)

func TestGCPMachineTypeCandidatesForClass(t *testing.T) {
	got := gcpMachineTypeCandidatesForClass("standard")
	want := []string{"c4-standard-32", "c3-standard-22", "n2-standard-32", "n2d-standard-32"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("standard=%v want %v", got, want)
	}
	if got := serverTypeForProviderClass("gcp", "beast"); got != "c4-standard-192" {
		t.Fatalf("gcp beast=%q", got)
	}
}

func TestGCPLabelsAreGoogleSafe(t *testing.T) {
	got := gcpLabels(map[string]string{
		"crabbox":      "true",
		"provider":     "gcp",
		"provider_key": "crabbox-steipete",
		"created_at":   "1777777777",
		"expires_at":   "1777779999",
		"ttl_secs":     "7200",
		"owner":        "Peter@OpenClaw.Org",
	})
	if got["owner"] != "peter_openclaw_org" {
		t.Fatalf("owner label=%q", got["owner"])
	}
	for key, want := range map[string]string{"created_at": "1777777777", "expires_at": "1777779999", "ttl_secs": "7200"} {
		if got[key] != want {
			t.Fatalf("numeric label value %s should be preserved, got %q", key, got[key])
		}
	}
	got = gcpLabels(map[string]string{"123": "456"})
	if got["x123"] != "456" {
		t.Fatalf("numeric label key should be prefixed while preserving value: %#v", got)
	}
}

func TestGCPInstanceToServer(t *testing.T) {
	server := gcpInstanceToServer("europe-west2-a", &computepb.Instance{
		Id:          proto.Uint64(123),
		Name:        proto.String("crabbox-blue"),
		Status:      proto.String("RUNNING"),
		MachineType: proto.String("https://www.googleapis.com/compute/v1/projects/p/zones/europe-west2-a/machineTypes/c4-standard-32"),
		Labels:      map[string]string{"crabbox": "true", "lease": "cbx_123"},
		NetworkInterfaces: []*computepb.NetworkInterface{{
			AccessConfigs: []*computepb.AccessConfig{{NatIP: proto.String("203.0.113.9")}},
		}},
	})
	if server.Provider != "gcp" || server.CloudID != "crabbox-blue" || server.PublicNet.IPv4.IP != "203.0.113.9" {
		t.Fatalf("server=%+v", server)
	}
	if server.ServerType.Name != "c4-standard-32" || server.Labels["zone"] != "europe-west2-a" {
		t.Fatalf("server metadata=%+v labels=%v", server.ServerType, server.Labels)
	}
}

func TestGCPInstanceToServerHandlesNilLabels(t *testing.T) {
	server := gcpInstanceToServer("europe-west2-a", &computepb.Instance{
		Name:   proto.String("crabbox-manual"),
		Status: proto.String("RUNNING"),
	})
	if server.Labels["zone"] != "europe-west2-a" {
		t.Fatalf("labels=%v", server.Labels)
	}
}

func TestGCPFirewallNameForNetwork(t *testing.T) {
	cases := map[string]string{
		"default":                                   "crabbox-ssh",
		"projects/p/global/networks/default":        "crabbox-ssh",
		"crabbox-ci":                                "crabbox-ssh-crabbox-ci",
		"projects/p/global/networks/123_custom":     "crabbox-ssh-net-123-custom",
		"projects/p/global/networks/custom network": "crabbox-ssh-custom-network",
	}
	for network, want := range cases {
		if got := gcpFirewallNameForNetwork(network); got != want {
			t.Fatalf("network %q firewall=%q want %q", network, got, want)
		}
	}
}

func TestGCPFirewallNameForPolicy(t *testing.T) {
	if got := gcpFirewallNameForPolicy("default", []string{"0.0.0.0/0"}, []string{"crabbox-ssh"}, []string{"2222", "22"}); got != "crabbox-ssh" {
		t.Fatalf("default policy firewall=%q", got)
	}
	a := gcpFirewallNameForPolicy("default", []string{"198.51.100.7/32"}, []string{"crabbox-ssh"}, []string{"2222", "22"})
	b := gcpFirewallNameForPolicy("default", []string{"203.0.113.8/32"}, []string{"crabbox-ssh"}, []string{"2222", "22"})
	if a == "crabbox-ssh" || b == "crabbox-ssh" || a == b {
		t.Fatalf("policy firewalls should be distinct: a=%q b=%q", a, b)
	}
	if got := gcpFirewallNameForPolicy("crabbox-ci", []string{"198.51.100.7/32"}, []string{"crabbox-ssh"}, []string{"2222", "22"}); !strings.HasPrefix(got, "crabbox-ssh-crabbox-ci-") {
		t.Fatalf("custom network policy firewall=%q", got)
	}
	got := gcpFirewallNameForPolicy("this-is-a-very-long-custom-network-name-that-would-fill-the-firewall-name", []string{"198.51.100.7/32"}, []string{"crabbox-ssh"}, []string{"2222", "22"})
	if len(got) > 63 {
		t.Fatalf("firewall name should fit GCP limit, got len=%d name=%q", len(got), got)
	}
}

func TestGCPClientDefaultsBlankTags(t *testing.T) {
	client, err := newGCPClientWithOptions(context.Background(), Config{
		GCPProject: "project",
		GCPZone:    "europe-west2-a",
		GCPTags:    []string{"  "},
	}, option.WithoutAuthentication(), option.WithEndpoint("http://127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(client.Tags, []string{"crabbox-ssh"}) {
		t.Fatalf("tags=%v", client.Tags)
	}
}

func TestGCPListCrabboxServersAggregatesZones(t *testing.T) {
	var gotPath string
	var gotFilter string
	var gotPartialSuccess string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFilter = r.URL.Query().Get("filter")
		gotPartialSuccess = r.URL.Query().Get("returnPartialSuccess")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"items": map[string]any{
				"zones/europe-west2-b": map[string]any{
					"instances": []map[string]any{{
						"name":   "crabbox-fallback-zone",
						"status": "RUNNING",
						"labels": map[string]string{"crabbox": "true", "lease": "cbx_333333333333"},
						"networkInterfaces": []map[string]any{{
							"accessConfigs": []map[string]string{{"natIP": "203.0.113.33"}},
						}},
					}},
				},
				"zones/us-central1-a": map[string]any{
					"instances": []map[string]any{{
						"name":   "crabbox-other-zone",
						"status": "RUNNING",
						"labels": map[string]string{"crabbox": "true", "lease": "cbx_444444444444"},
					}},
				},
			},
		}); err != nil {
			t.Fatal(err)
		}
	}))
	defer server.Close()

	instances, err := gcpcompute.NewInstancesRESTClient(context.Background(), option.WithoutAuthentication(), option.WithEndpoint(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	defer instances.Close()
	client := &GCPClient{Project: "project", Zone: "europe-west2-a", instances: instances}
	servers, err := client.ListCrabboxServers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/compute/v1/projects/project/aggregated/instances" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotFilter != "labels.crabbox = true" || gotPartialSuccess != "true" {
		t.Fatalf("filter=%q returnPartialSuccess=%q", gotFilter, gotPartialSuccess)
	}
	if len(servers) != 2 {
		t.Fatalf("servers=%v", servers)
	}
	if servers[0].Name != "crabbox-fallback-zone" || servers[0].Labels["zone"] != "europe-west2-b" || servers[0].PublicNet.IPv4.IP != "203.0.113.33" {
		t.Fatalf("fallback server=%#v", servers[0])
	}
	if servers[1].Name != "crabbox-other-zone" || servers[1].Labels["zone"] != "us-central1-a" {
		t.Fatalf("other server=%#v", servers[1])
	}
}

func TestGCPFallbackProvisioningErrorIncludesUnavailableMachineTypes(t *testing.T) {
	cases := []error{
		&googleapi.Error{
			Code:    http.StatusBadRequest,
			Message: "Invalid value for field 'resource.machineType': 'zones/us-central1-a/machineTypes/c4-standard-192'. The referenced resource does not exist.",
		},
		&googleapi.Error{
			Code:    http.StatusNotFound,
			Message: "The resource 'projects/p/zones/us-central1-a/machineTypes/c4-standard-192' was not found",
		},
		fmt.Errorf("create gcp instance: %w", &googleapi.Error{
			Code:    http.StatusBadRequest,
			Message: "Invalid value for field 'resource.machineType': 'zones/us-central1-a/machineTypes/c4-standard-192'. The referenced resource does not exist.",
		}),
		&googleapi.Error{
			Code:    http.StatusForbidden,
			Message: "Quota 'CPUS' exceeded. Limit: 24.0 in region europe-west2.",
		},
		&googleapi.Error{
			Code: http.StatusForbidden,
			Errors: []googleapi.ErrorItem{{
				Reason:  "rateLimitExceeded",
				Message: "Rate Limit Exceeded",
			}},
		},
	}
	for _, err := range cases {
		if !isGCPFallbackProvisioningError(err) {
			t.Fatalf("expected fallback-eligible error: %v", err)
		}
	}
	if isGCPFallbackProvisioningError(&googleapi.Error{Code: http.StatusBadRequest, Message: "invalid labels"}) {
		t.Fatal("unrelated bad request should not be fallback-eligible")
	}
	if isGCPFallbackProvisioningError(&googleapi.Error{Code: http.StatusForbidden, Message: "Required 'compute.instances.create' permission for project"}) {
		t.Fatal("permission 403 should not be fallback-eligible")
	}
	if isGCPFallbackProvisioningError(&googleapi.Error{Code: http.StatusForbidden, Body: "missing quota_project_id for credentials"}) {
		t.Fatal("quota project auth/config 403 should not be fallback-eligible")
	}
}
