package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
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
			keys, _ := body["ssh_keys"].([]any)
			if len(keys) != 1 || keys[0].(float64) != 123 {
				t.Fatalf("ssh_keys=%v", body["ssh_keys"])
			}
			tags, _ := body["tags"].([]any)
			if len(tags) == 0 {
				t.Fatalf("tags missing: %v", body)
			}
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"droplet":{"id":456,"name":"crabbox-blue","status":"new","tags":["crabbox"]}}`))
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
	for _, req := range requests {
		if req.Auth != "Bearer secret-token" {
			t.Fatalf("%s %s auth=%q", req.Method, req.Path, req.Auth)
		}
		if req.Method == http.MethodPost && req.Path == "/tags" {
			t.Fatalf("create performed redundant tag request: requests=%v", requests)
		}
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
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"tag":{"name":"ok"}}`))
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

	err = client.rollbackCreatedSSHKey("crabbox-cbx-abcdef123456", context.Canceled)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("rollback err=%v", err)
	}
	if !deleteKey {
		t.Fatal("new ssh key was not rolled back")
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
		case r.Method == http.MethodGet && r.URL.Path == "/droplets":
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
	page := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Fatalf("auth=%q", r.Header.Get("Authorization"))
		}
		if r.URL.Query().Get("tag_name") != "crabbox" {
			t.Fatalf("tag_name=%q", r.URL.Query().Get("tag_name"))
		}
		page++
		if page == 1 {
			_, _ = w.Write([]byte(`{"droplets":[{"id":1,"name":"owned","tags":["crabbox","crabbox:provider:digitalocean","crabbox:lease:cbx_1","crabbox:slug:one","crabbox:target:linux"]},{"id":2,"name":"foreign","tags":["crabbox"]}],"links":{"pages":{"next":"yes"}}}`))
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
	if len(droplets) != 2 || droplets[0].ID != 1 || droplets[1].ID != 3 {
		t.Fatalf("droplets=%v", droplets)
	}
}
