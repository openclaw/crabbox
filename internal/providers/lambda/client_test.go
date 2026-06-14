package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestClientUsesBearerAndDataEnvelope(t *testing.T) {
	t.Setenv(tokenEnv, "lambda-secret-token")
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/api/v1/regions" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":[{"name":"us-west-1"}]}`))
	}))
	defer server.Close()

	client, err := newClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL + "/api/v1"
	regions, err := client.ListRegions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer lambda-secret-token" {
		t.Fatalf("Authorization=%q", gotAuth)
	}
	if len(regions) != 1 || regions[0].Name != "us-west-1" {
		t.Fatalf("regions=%#v", regions)
	}
}

func TestClientListInstanceTypesAcceptsMapEnvelope(t *testing.T) {
	t.Setenv(tokenEnv, "lambda-secret-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/instance-types" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"data":{"gpu_1x_a10":{"instance_type":{"name":"gpu_1x_a10","description":"A10"},"regions_with_capacity_available":["us-west-1"]}}}`))
	}))
	defer server.Close()

	client, err := newClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL + "/api/v1"
	types, err := client.ListInstanceTypes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(types) != 1 || types[0].Name != "gpu_1x_a10" || types[0].Description != "A10" || len(types[0].RegionsWithCapacityAvailable) != 1 || types[0].RegionsWithCapacityAvailable[0] != "us-west-1" {
		t.Fatalf("types=%#v", types)
	}
}

func TestClientPreservesErrorCodeAndRedactsSecrets(t *testing.T) {
	t.Setenv(tokenEnv, "lambda-secret-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"code":"global/invalid-api-key","message":"token lambda-secret-token user_data=boot private_key=-----BEGIN PRIVATE KEY-----abc-----END PRIVATE KEY----- jupyter_url=https://example.test/?token=abc","suggestion":"replace api_key=lambda-secret-token"}}`))
	}))
	defer server.Close()

	client, err := newClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	err = client.do(context.Background(), http.MethodGet, "/regions", nil, &[]Region{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err=%T %v", err, err)
	}
	if apiErr.Code != "global/invalid-api-key" {
		t.Fatalf("Code=%q", apiErr.Code)
	}
	combined := apiErr.Error() + apiErr.Body + apiErr.Suggestion
	for _, secret := range []string{"lambda-secret-token", "boot", "BEGIN PRIVATE KEY", "token=abc"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("error leaked %q: %s", secret, combined)
		}
	}
}

func TestLaunchRequestShape(t *testing.T) {
	req := LaunchInstanceRequest{
		RegionName:          "us-west-1",
		InstanceTypeName:    "gpu_1x_a10",
		Quantity:            1,
		SSHKeyNames:         []string{"crabbox-cbx_123"},
		ImageFamily:         "lambda-stack-24-04",
		UserData:            "cloud-config",
		FirewallRulesetName: "crabbox",
		FileSystemNames:     []string{"cache"},
		FileSystemMounts:    []FilesystemMountRequest{{Name: "cache", MountPath: "/mnt/cache"}},
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"region_name", "instance_type_name", "quantity", "ssh_key_names", "image_family", "user_data", "firewall_ruleset_name", "file_system_names", "file_system_mounts"} {
		if !strings.Contains(string(data), `"`+key+`"`) {
			t.Fatalf("request missing %s: %s", key, data)
		}
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"name", "tags"} {
		if _, ok := got[key]; ok {
			t.Fatalf("request should not include unsupported %s: %s", key, data)
		}
	}
}
