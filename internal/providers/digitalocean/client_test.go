package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestDigitalOceanClientCreateDropletRequestShape(t *testing.T) {
	var requests []struct {
		Method string
		Path   string
		Body   map[string]any
		Auth   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		requests = append(requests, struct {
			Method string
			Path   string
			Body   map[string]any
			Auth   string
		}{Method: r.Method, Path: r.URL.RequestURI(), Body: body, Auth: r.Header.Get("Authorization")})

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			if got := body["region"]; got != "sfo3" {
				t.Fatalf("region=%v", got)
			}
			if got := body["size"]; got != "s-2vcpu-2gb" {
				t.Fatalf("size=%v", got)
			}
			if got := body["image"]; got != "ubuntu-24-04-x64" {
				t.Fatalf("image=%v", got)
			}
			if got := body["vpc_uuid"]; got != "vpc-123" {
				t.Fatalf("vpc_uuid=%v", got)
			}
			if _, ok := body["monitoring"]; ok {
				t.Fatalf("monitoring agent must not be enabled: %v", body)
			}
			keys, _ := body["ssh_keys"].([]any)
			if len(keys) != 1 || keys[0].(float64) != 123 {
				t.Fatalf("ssh_keys=%v", body["ssh_keys"])
			}
			tags, _ := body["tags"].([]any)
			if len(tags) == 0 {
				t.Fatalf("tags missing: %v", body)
			}
			if !containsAnyTag(tags, tagCrabbox) {
				t.Fatalf("tags missing ownership tag: %v", tags)
			}
			w.WriteHeader(http.StatusAccepted)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"droplet": map[string]any{
					"id":     456,
					"name":   body["name"],
					"status": "new",
					"tags":   []string{"Crabbox"},
				},
			}); err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "secret-token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-2vcpu-2gb"
	cfg.DigitalOcean.Region = "sfo3"
	cfg.DigitalOcean.Image = "ubuntu-24-04-x64"
	cfg.DigitalOcean.VPCUUID = "vpc-123"

	if _, err := client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(requests) == 0 {
		t.Fatal("no requests captured")
	}
	var tagRequests int
	for _, req := range requests {
		if req.Auth != "Bearer secret-token" {
			t.Fatalf("%s %s auth=%q", req.Method, req.Path, req.Auth)
		}
		if strings.HasPrefix(req.Path, "/tags") {
			tagRequests++
		}
		if req.Method == http.MethodGet && req.Path == "/tags?page=1&per_page=200" {
			t.Fatalf("create scanned all account tags: requests=%v", requests)
		}
	}
	if tagRequests != 0 {
		t.Fatalf("normal create tag requests=%d, want 0: requests=%v", tagRequests, requests)
	}
}

func TestDigitalOceanClientAccountIDPrefersTeamContext(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/account" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"account":{"uuid":"user-123","team":{"uuid":"team-456"}}}`))
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	accountID, err := client.AccountID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "team:team-456" {
		t.Fatalf("accountID=%q", accountID)
	}
}

func containsAnyTag(tags []any, want string) bool {
	for _, tag := range tags {
		if tag == want {
			return true
		}
	}
	return false
}

func writeDigitalOceanTagList(t *testing.T, w http.ResponseWriter, names ...string) {
	t.Helper()
	tags := make([]digitalOceanTag, 0, len(names))
	for _, name := range names {
		tags = append(tags, digitalOceanTag{Name: name})
	}
	if err := json.NewEncoder(w).Encode(map[string]any{
		"tags":  tags,
		"links": map[string]any{"pages": map[string]any{}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestDigitalOceanClientCreateDropletRollsBackKeyOnSemanticTagCollision(t *testing.T) {
	var deleteKey bool
	var keyListCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			keyListCalls++
			if keyListCalls == 1 {
				_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			http.Error(w, "tag conflict", http.StatusUnprocessableEntity)
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w, "Crabbox:Slug:Blue")
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"

	_, err = client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Now())
	if err == nil || !strings.Contains(err.Error(), `conflicts with existing account tag "Crabbox:Slug:Blue"`) {
		t.Fatalf("CreateDroplet err=%v", err)
	}
	if !deleteKey {
		t.Fatal("semantic tag collision did not roll back its SSH key")
	}
}

func TestDigitalOceanClientCreateDropletRetriesWithCanonicalTags(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	canonicalLeaseTag := "Crabbox:Lease:" + leaseID
	var createCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			createCalls++
			if createCalls == 1 {
				http.Error(w, "tag conflict", http.StatusUnprocessableEntity)
				return
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			tags, _ := body["tags"].([]any)
			if !containsAnyTag(tags, "Crabbox") || !containsAnyTag(tags, canonicalLeaseTag) {
				t.Fatalf("retry tags=%v", tags)
			}
			w.WriteHeader(http.StatusAccepted)
			if err := json.NewEncoder(w).Encode(map[string]any{
				"droplet": map[string]any{"id": 456, "name": body["name"], "status": "new", "tags": tags},
			}); err != nil {
				t.Fatal(err)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w, "Crabbox", canonicalLeaseTag)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"

	item, err := client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", leaseID, "blue", false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if createCalls != 2 || item.ID != 456 {
		t.Fatalf("createCalls=%d droplet=%#v", createCalls, item)
	}
}

func TestDigitalOceanClientReplaceDropletTagsDetachesObsoleteCrabboxTags(t *testing.T) {
	var requests []struct {
		Method string
		Path   string
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, struct {
			Method string
			Path   string
		}{Method: r.Method, Path: r.URL.RequestURI()})
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/tags/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			var body struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"tag":{"name":"` + body.Name + `"}}`))
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/tags/") && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/tags/") && strings.HasSuffix(r.URL.Path, "/resources"):
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	err = client.ReplaceDropletTags(
		context.Background(),
		42,
		[]string{tagCrabbox, "crabbox:lease:cbx_abcdef123456", "crabbox:state:running", "crabbox:last_touched_at:100", "other"},
		[]string{tagCrabbox, "crabbox:lease:cbx_abcdef123456", "crabbox:state:ready", "crabbox:last_touched_at:200"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var created, attached, detached []string
	for _, req := range requests {
		switch {
		case req.Method == http.MethodPost && req.Path == "/tags":
			created = append(created, req.Path)
		case req.Method == http.MethodPost && strings.HasSuffix(req.Path, "/resources"):
			attached = append(attached, req.Path)
		case req.Method == http.MethodDelete && strings.HasSuffix(req.Path, "/resources"):
			detached = append(detached, req.Path)
		}
	}
	if len(created) != 2 || len(attached) != 2 {
		t.Fatalf("created=%v attached=%v requests=%v", created, attached, requests)
	}
	for _, path := range append(created, attached...) {
		if strings.Contains(path, "crabbox:lease:cbx_abcdef123456") {
			t.Fatalf("unchanged lease tag was recreated or reattached: requests=%v", requests)
		}
	}
	if len(detached) != 2 ||
		!slicesContainSubstring(detached, "crabbox:state:running") ||
		!slicesContainSubstring(detached, "crabbox:last_touched_at:100") {
		t.Fatalf("detached=%v requests=%v", detached, requests)
	}
}

func TestDigitalOceanClientReplaceDropletTagsUsesCanonicalTagName(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.RequestURI())
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags/crabbox:state:ready":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			http.Error(w, "already exists", http.StatusUnprocessableEntity)
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"tags":[{"name":"Crabbox:State:Ready"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/tags/Crabbox:State:Ready/resources":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if err := client.ReplaceDropletTags(
		context.Background(),
		42,
		[]string{tagCrabbox},
		[]string{tagCrabbox, "crabbox:state:ready"},
	); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 4 ||
		requests[0] != "GET /tags/crabbox:state:ready" ||
		requests[1] != "POST /tags" ||
		requests[2] != "GET /tags?page=1&per_page=200" ||
		requests[3] != "POST /tags/Crabbox:State:Ready/resources" {
		t.Fatalf("requests=%v", requests)
	}
}

func TestDigitalOceanClientEnsureTagRejectsUnconfirmedConflict(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags/crabbox:state:ready":
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			http.Error(w, "unprocessable", http.StatusUnprocessableEntity)
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"tags":[],"links":{"pages":{}}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if _, err := client.EnsureTag(context.Background(), "crabbox:state:ready", map[string]string{}); err == nil {
		t.Fatal("EnsureTag unexpectedly suppressed unconfirmed 422")
	}
}

func TestDigitalOceanClientEnsureTagRejectsSemanticCanonicalCollision(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/tags/crabbox:slug:blue":
			_, _ = w.Write([]byte(`{"tag":{"name":"Crabbox:Slug:Blue"}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if _, err := client.EnsureTag(context.Background(), "crabbox:slug:blue", map[string]string{}); err == nil {
		t.Fatal("EnsureTag accepted semantic canonical collision")
	}
}

func slicesContainSubstring(values []string, substring string) bool {
	for _, value := range values {
		if strings.Contains(value, substring) {
			return true
		}
	}
	return false
}

func TestDigitalOceanClientReplaceDropletTagsSkipsUnchangedSet(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	tags := []string{tagCrabbox, "crabbox:lease:cbx_1", "crabbox:state:ready"}
	if err := client.ReplaceDropletTags(context.Background(), 42, tags, append([]string(nil), tags...)); err != nil {
		t.Fatal(err)
	}
	if requestCount != 0 {
		t.Fatalf("requestCount=%d", requestCount)
	}
}

func TestDigitalOceanClientCreateDropletRollsBackNewSSHKeyOnCreateFailure(t *testing.T) {
	var deleteKey bool
	var keyListCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			keyListCalls++
			w.WriteHeader(http.StatusOK)
			if keyListCalls == 1 {
				_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			http.Error(w, "create denied", http.StatusForbidden)
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	_, err = client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Now())
	if err == nil {
		t.Fatal("CreateDroplet succeeded")
	}
	if !deleteKey {
		t.Fatal("new ssh key was not rolled back")
	}
}

func TestDigitalOceanClientCreateDropletPreservesKeyOnAmbiguousFailure(t *testing.T) {
	var deleteKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/keys":
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w)
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			_, _ = w.Write([]byte(`{"droplets":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.reconcileTimeout = 20 * time.Millisecond
	client.reconcileInterval = time.Millisecond
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"

	_, err = client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Now())
	var ambiguous *ambiguousDropletCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("CreateDroplet err=%v, want ambiguousDropletCreateError", err)
	}
	if !ambiguous.keyOwnershipKnown || !ambiguous.keyCreated || ambiguous.keyID != 123 {
		t.Fatalf("ambiguous key identity=%#v", ambiguous)
	}
	if deleteKey {
		t.Fatal("ambiguous create deleted its SSH key")
	}
}

func TestDigitalOceanClientCreateDropletStopsReconciliationOnLeaseTagCollision(t *testing.T) {
	var deleteKey bool
	var dropletLookup bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/keys":
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w, "Crabbox:Lease:CBX_ABCDEF123456")
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			dropletLookup = true
			t.Fatal("semantic lease collision reached Droplet reconciliation")
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"

	_, err = client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "blue", false, time.Now())
	var ambiguous *ambiguousDropletCreateError
	if !errors.As(err, &ambiguous) || !strings.Contains(err.Error(), "conflicts with existing account tag") {
		t.Fatalf("CreateDroplet err=%v", err)
	}
	if dropletLookup {
		t.Fatal("semantic lease collision queried Droplets")
	}
	if deleteKey {
		t.Fatal("ambiguous create deleted its SSH key")
	}
}

func TestDigitalOceanClientRollbackCreatedSSHKeyUsesFreshContext(t *testing.T) {
	var deleteKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() != nil {
			t.Fatalf("cleanup request used canceled context: %v", r.Context().Err())
		}
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL

	err = client.rollbackCreatedSSHKey(sshKey{ID: 123, Name: "crabbox-cbx-abcdef123456"}, context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("rollback err=%v", err)
	}
	if !deleteKey {
		t.Fatal("new ssh key was not rolled back")
	}
}

func TestDigitalOceanClientRollbackCreatedSSHKeyReportsRetryableOwnership(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	cause := errors.New("droplet create failed")
	err = client.rollbackCreatedSSHKey(sshKey{ID: 123, Name: "crabbox-cbx-abcdef123456"}, cause)
	var cleanup *sshKeyCleanupError
	if !errors.As(err, &cleanup) || !errors.Is(err, cause) || cleanup.keyID != 123 {
		t.Fatalf("rollback err=%v, want sshKeyCleanupError wrapping cause", err)
	}
}

func TestDigitalOceanClientCreateDropletReconcilesLostResponse(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	slug := "lost-response"
	name := core.LeaseProviderName(leaseID, slug)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	tags := leaseTags(cfg, leaseID, slug, "provisioning", false, time.Now())
	var deleteKey bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = conn.Close()
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w)
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			if r.URL.Query().Get("type") == "gpus" {
				if got := r.URL.Query().Get("tag_name"); got != "" {
					t.Fatalf("gpu tag_name=%q", got)
				}
				_, _ = w.Write([]byte(`{"droplets":[],"links":{"pages":{}}}`))
				return
			}
			if got := r.URL.Query().Get("tag_name"); got != encodeTagKV("lease", leaseID) {
				t.Fatalf("tag_name=%q", got)
			}
			payload, err := json.Marshal(map[string]any{
				"droplets": []droplet{{ID: 456, Name: name, Status: "new", Tags: tags}},
				"links":    map[string]any{"pages": map[string]any{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(payload)
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL

	item, err := client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", leaseID, slug, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != 456 || item.Name != name {
		t.Fatalf("droplet=%#v", item)
	}
	if deleteKey {
		t.Fatal("reconciled create deleted its SSH key")
	}
}

func TestDigitalOceanClientCreateDropletReconcilesEmptySuccessBody(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	slug := "empty-response"
	name := core.LeaseProviderName(leaseID, slug)
	canonicalLeaseTag := "Crabbox:Lease:" + leaseID
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	tags := leaseTags(cfg, leaseID, slug, "provisioning", false, time.Now())
	var deleteKey bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/keys":
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w, canonicalLeaseTag)
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			if r.URL.Query().Get("type") == "gpus" {
				_, _ = w.Write([]byte(`{"droplets":[],"links":{"pages":{}}}`))
				return
			}
			if got := r.URL.Query().Get("tag_name"); got != canonicalLeaseTag {
				t.Fatalf("tag_name=%q", got)
			}
			payload, err := json.Marshal(map[string]any{
				"droplets": []droplet{{ID: 456, Name: name, Status: "new", Tags: tags}},
				"links":    map[string]any{"pages": map[string]any{}},
			})
			if err != nil {
				t.Fatal(err)
			}
			_, _ = w.Write(payload)
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL

	item, err := client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", leaseID, slug, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != 456 || item.Name != name {
		t.Fatalf("droplet=%#v", item)
	}
	if deleteKey {
		t.Fatal("reconciled create deleted its SSH key")
	}
}

func TestDigitalOceanClientCreateDropletPreservesKeyWhenEmptySuccessCannotReconcile(t *testing.T) {
	var deleteKey bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/keys":
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/droplets":
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{}`))
		case r.Method == http.MethodGet && r.URL.Path == "/tags":
			writeDigitalOceanTagList(t, w)
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
			_, _ = w.Write([]byte(`{"droplets":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/account/keys/123":
			deleteKey = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.reconcileTimeout = 20 * time.Millisecond
	client.reconcileInterval = time.Millisecond
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"

	_, err = client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", "cbx_abcdef123456", "empty-response", false, time.Now())
	var ambiguous *ambiguousDropletCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("CreateDroplet err=%v, want ambiguousDropletCreateError", err)
	}
	if deleteKey {
		t.Fatal("indeterminate create deleted its SSH key")
	}
}

func TestDigitalOceanClientCreateDropletReconcilesTruncatedSuccessBody(t *testing.T) {
	leaseID := "cbx_abcdef123456"
	slug := "truncated-response"
	name := core.LeaseProviderName(leaseID, slug)
	cfg := core.BaseConfig()
	cfg.Provider = providerName
	cfg.TargetOS = core.TargetLinux
	cfg.ServerType = "s-1vcpu-1gb"
	tags := leaseTags(cfg, leaseID, slug, "provisioning", false, time.Now())
	var deleteKey bool

	client := &digitalOceanClient{
		token:   "token",
		baseURL: "https://api.digitalocean.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			response := func(status int, body io.ReadCloser) *http.Response {
				return &http.Response{
					StatusCode: status,
					Header:     make(http.Header),
					Body:       body,
					Request:    req,
				}
			}
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/tags":
				return response(http.StatusOK, io.NopCloser(strings.NewReader(`{"tags":[],"links":{"pages":{}}}`))), nil
			case req.Method == http.MethodGet && req.URL.Path == "/account/keys":
				return response(http.StatusOK, io.NopCloser(strings.NewReader(`{"ssh_keys":[],"links":{"pages":{}}}`))), nil
			case req.Method == http.MethodPost && req.URL.Path == "/account/keys":
				return response(http.StatusCreated, io.NopCloser(strings.NewReader(`{"ssh_key":{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}}`))), nil
			case req.Method == http.MethodPost && req.URL.Path == "/droplets":
				return response(http.StatusAccepted, io.NopCloser(&errorAfterReader{
					data: []byte(`{"droplet":{"id":456`),
					err:  io.ErrUnexpectedEOF,
				})), nil
			case req.Method == http.MethodGet && req.URL.Path == "/droplets":
				if req.URL.Query().Get("type") == "gpus" {
					if got := req.URL.Query().Get("tag_name"); got != "" {
						t.Fatalf("gpu tag_name=%q", got)
					}
					return response(http.StatusOK, io.NopCloser(strings.NewReader(`{"droplets":[],"links":{"pages":{}}}`))), nil
				}
				if got := req.URL.Query().Get("tag_name"); got != encodeTagKV("lease", leaseID) {
					t.Fatalf("tag_name=%q", got)
				}
				payload, err := json.Marshal(map[string]any{
					"droplets": []droplet{{ID: 456, Name: name, Status: "new", Tags: tags}},
					"links":    map[string]any{"pages": map[string]any{}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return response(http.StatusOK, io.NopCloser(strings.NewReader(string(payload)))), nil
			case req.Method == http.MethodDelete && req.URL.Path == "/account/keys/123":
				deleteKey = true
				return response(http.StatusNoContent, http.NoBody), nil
			default:
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.RequestURI())
				return nil, nil
			}
		})},
	}

	item, err := client.CreateDroplet(context.Background(), cfg, "ssh-ed25519 test", leaseID, slug, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if item.ID != 456 || item.Name != name {
		t.Fatalf("droplet=%#v", item)
	}
	if deleteKey {
		t.Fatal("reconciled create deleted its SSH key")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

type errorAfterReader struct {
	data []byte
	err  error
}

func (r *errorAfterReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func TestDigitalOceanClientDeleteDropletPreservesTruncatedNotFoundStatus(t *testing.T) {
	client := &digitalOceanClient{
		token:   "token",
		baseURL: "https://api.digitalocean.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNotFound,
				Header:     make(http.Header),
				Body: io.NopCloser(&errorAfterReader{
					data: []byte(`{"id":"not_found"`),
					err:  io.ErrUnexpectedEOF,
				}),
				Request: req,
			}, nil
		})},
	}

	if err := client.DeleteDroplet(context.Background(), 456); err != nil {
		t.Fatalf("DeleteDroplet err=%v", err)
	}
}

func TestDigitalOceanClientEnsureSSHKeyReconcilesTruncatedSuccessBody(t *testing.T) {
	var listCalls int
	client := &digitalOceanClient{
		token:   "token",
		baseURL: "https://api.digitalocean.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			response := func(status int, body io.ReadCloser) *http.Response {
				return &http.Response{
					StatusCode: status,
					Header:     make(http.Header),
					Body:       body,
					Request:    req,
				}
			}
			switch {
			case req.Method == http.MethodGet && req.URL.Path == "/account/keys":
				listCalls++
				if listCalls == 1 {
					return response(http.StatusOK, io.NopCloser(strings.NewReader(`{"ssh_keys":[],"links":{"pages":{}}}`))), nil
				}
				return response(http.StatusOK, io.NopCloser(strings.NewReader(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))), nil
			case req.Method == http.MethodPost && req.URL.Path == "/account/keys":
				return response(http.StatusCreated, io.NopCloser(&errorAfterReader{
					data: []byte(`{"ssh_key":{"id":123`),
					err:  io.ErrUnexpectedEOF,
				})), nil
			default:
				t.Fatalf("unexpected request %s %s", req.Method, req.URL.RequestURI())
				return nil, nil
			}
		})},
	}

	key, created, err := client.EnsureSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 test")
	if err != nil {
		t.Fatal(err)
	}
	if !created || key.ID != 123 || listCalls != 2 {
		t.Fatalf("key=%#v created=%v listCalls=%d", key, created, listCalls)
	}
}

func TestDigitalOceanClientEnsureSSHKeyReconcilesEmptySuccessBody(t *testing.T) {
	var listCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/account/keys":
			listCalls++
			if listCalls == 1 {
				_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
				return
			}
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 test"}],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	key, created, err := client.EnsureSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 test")
	if err != nil {
		t.Fatal(err)
	}
	if !created || key.ID != 123 || listCalls != 2 {
		t.Fatalf("key=%#v created=%v listCalls=%d", key, created, listCalls)
	}
}

func TestDigitalOceanClientEnsureSSHKeyRejectsMismatchedPublicKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":123,"name":"crabbox-cbx-abcdef123456","fingerprint":"fp","public_key":"ssh-ed25519 different"}],"links":{"pages":{}}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	_, _, err = client.EnsureSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 expected")
	if err == nil || !strings.Contains(err.Error(), "exists with different public key") {
		t.Fatalf("EnsureSSHKey err=%v", err)
	}
	var ambiguous *ambiguousSSHKeyCreateError
	if errors.As(err, &ambiguous) {
		t.Fatalf("EnsureSSHKey err=%v, definitive conflict classified as ambiguous", err)
	}
}

func TestDigitalOceanClientEnsureSSHKeySelectsMatchingDuplicateName(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":122,"name":"crabbox-cbx-abcdef123456","public_key":"ssh-ed25519 different"},{"id":123,"name":"crabbox-cbx-abcdef123456","public_key":"ssh-ed25519 expected"}],"links":{"pages":{}}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL

	key, created, err := client.EnsureSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 expected")
	if err != nil {
		t.Fatal(err)
	}
	if created || key.ID != 123 {
		t.Fatalf("key=%#v created=%v", key, created)
	}
}

func TestDigitalOceanClientFindSSHKeyRejectsDuplicatePublicKeyMatches(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ssh_keys":[{"id":122,"name":"crabbox-cbx-abcdef123456","public_key":"ssh-ed25519 expected"},{"id":123,"name":"crabbox-cbx-abcdef123456","public_key":"ssh-ed25519 expected"}],"links":{"pages":{}}}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL

	_, _, err = client.FindSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 expected")
	if err == nil || !strings.Contains(err.Error(), "multiple entries matching") {
		t.Fatalf("FindSSHKey err=%v", err)
	}
}

func TestDigitalOceanClientEnsureSSHKeyPreservesAmbiguousCreate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.RequestURI(), "/account/keys"):
			_, _ = w.Write([]byte(`{"ssh_keys":[],"links":{"pages":{}}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/account/keys":
			http.Error(w, "temporary failure", http.StatusInternalServerError)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	client.reconcileTimeout = 20 * time.Millisecond
	client.reconcileInterval = time.Millisecond
	_, _, err = client.EnsureSSHKey(context.Background(), "crabbox-cbx-abcdef123456", "ssh-ed25519 test")
	var ambiguous *ambiguousSSHKeyCreateError
	if !errors.As(err, &ambiguous) {
		t.Fatalf("EnsureSSHKey err=%v, want ambiguousSSHKeyCreateError", err)
	}
}

func TestNewDigitalOceanClientRequiresToken(t *testing.T) {
	old := os.Getenv("DIGITALOCEAN_TOKEN")
	t.Cleanup(func() { _ = os.Setenv("DIGITALOCEAN_TOKEN", old) })
	t.Setenv("DIGITALOCEAN_TOKEN", "")
	if _, err := newDigitalOceanClient(core.Runtime{}); err == nil || !strings.Contains(err.Error(), "DIGITALOCEAN_TOKEN is required") {
		t.Fatalf("newDigitalOceanClient err=%v", err)
	}
}

func TestListCrabboxDropletsFiltersAndPaginates(t *testing.T) {
	standardPage := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("auth=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("tag_name") != "" {
			t.Fatalf("tag_name=%q", r.URL.Query().Get("tag_name"))
		}
		if r.URL.Query().Get("type") == "gpus" {
			_, _ = w.Write([]byte(`{"droplets":[{"id":4,"name":"gpu-owned","tags":["crabbox","crabbox:provider:digitalocean","crabbox:lease:cbx_4","crabbox:slug:gpu","crabbox:target:linux"]}],"links":{"pages":{}}}`))
			return
		}
		standardPage++
		if standardPage == 1 {
			_, _ = w.Write([]byte(`{"droplets":[{"id":1,"name":"owned","tags":["Crabbox","Crabbox:Provider:DigitalOcean","Crabbox:Lease:cbx_1","Crabbox:Slug:one","Crabbox:Target:Linux"]},{"id":2,"name":"foreign","tags":["Crabbox"]}],"links":{"pages":{"next":"yes"}}}`))
			return
		}
		_, _ = w.Write([]byte(`{"droplets":[{"id":3,"name":"owned2","tags":["crabbox","crabbox:provider:digitalocean","crabbox:lease:cbx_2","crabbox:slug:two","crabbox:target:linux"]}],"links":{"pages":{}}}`))
	}))
	defer server.Close()
	t.Setenv("DIGITALOCEAN_TOKEN", "token")
	client, err := newDigitalOceanClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	droplets, err := client.ListCrabboxDroplets(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(droplets) != 3 || droplets[0].ID != 1 || droplets[1].ID != 3 || droplets[2].ID != 4 {
		t.Fatalf("droplets=%v", droplets)
	}
}
