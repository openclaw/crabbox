package lambda

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
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
		_, _ = w.Write([]byte(`{"error":{"code":"global/invalid-api-key","message":"token lambda-secret-token user_data=#cloud-config\nruncmd:\n- export TS_AUTHKEY=tskey-secret\nprivate_key=-----BEGIN PRIVATE KEY-----abc-----END PRIVATE KEY-----\njupyter_url=https://example.test/?token=abc","suggestion":"replace api_key=lambda-secret-token"}}`))
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
	for _, secret := range []string{"lambda-secret-token", "cloud-config", "TS_AUTHKEY", "tskey-secret", "BEGIN PRIVATE KEY", "token=abc"} {
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

type adoptionRoundTripper func(*http.Request) (*http.Response, error)

func (f adoptionRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func assertAdoptionRequest(t *testing.T, req *http.Request, ctx context.Context, endpoint, body string, headers http.Header) {
	t.Helper()
	if req.Context() != ctx || req.Method != http.MethodPost || req.URL.String() != endpoint {
		t.Fatalf("request context/method/URL mismatch: %s %s", req.Method, req.URL)
	}
	if !reflect.DeepEqual(req.Header, headers) {
		t.Fatalf("headers=%v want %v", req.Header, headers)
	}
	if req.ContentLength != int64(len(body)) || (req.Body == nil) != (body == "") {
		t.Fatalf("body metadata length=%d nil=%v want length=%d", req.ContentLength, req.Body == nil, len(body))
	}
	if body == "" {
		if req.GetBody != nil {
			t.Fatal("nil body gained replay")
		}
		return
	}
	data, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("body=%q want %q", data, body)
	}
	if req.GetBody == nil {
		t.Fatal("encoded body lost replay")
	}
	replay, err := req.GetBody()
	if err != nil {
		t.Fatal(err)
	}
	defer replay.Close()
	data, err = io.ReadAll(replay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != body {
		t.Fatalf("replay=%q want %q", data, body)
	}
}

func TestJSONRequestAdoptionEnvelope(t *testing.T) {
	type key struct{}
	ctx := context.WithValue(context.Background(), key{}, "capture")
	var typedNil *struct{ Value string }
	const base = "https://api.example.test/base"
	sentinel := errors.New("synthetic captured transport stop")
	for _, tc := range []struct {
		name        string
		body        any
		want        string
		query, fail bool
	}{
		{name: "nil"},
		{name: "typed nil", body: typedNil, want: "null\n"},
		{name: "JSON bytes", body: map[string]string{"message": "<&>"}, want: "{\"message\":\"\\u003c\\u0026\\u003e\"}\n"},
		{name: "query without body", query: true},
		{name: "transport error", fail: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			endpoint := "/records"
			if tc.query {
				endpoint += "?limit=2&prefix=two+words"
			}

			headers := http.Header{"Authorization": []string{"Bearer synthetic-token"}}
			headers.Set("Accept", "application/json")
			if tc.body != nil {
				headers.Set("Content-Type", "application/json")
			}
			transport := &http.Client{Transport: adoptionRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				assertAdoptionRequest(t, req, ctx, base+endpoint, tc.want, headers)
				if tc.fail {
					return nil, sentinel
				}
				return &http.Response{StatusCode: 204, Header: http.Header{"X-Capture": []string{"yes"}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})}
			c := &Client{baseURL: base, token: "synthetic-token", client: transport}
			var gotHeaders http.Header
			err := c.do(ctx, http.MethodPost, endpoint, tc.body, nil)
			if tc.fail {
				if !errors.Is(err, sentinel) || gotHeaders != nil {
					t.Fatalf("error/headers=%v %v", err, gotHeaders)
				}
			} else if err != nil {
				t.Fatal(err)
			}

			if calls != 1 {
				t.Fatalf("calls=%d", calls)
			}
		})
	}
}
