package digitalocean

import (
	"context"
	"encoding/json"
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
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"tag":{"name":"ok"}}`))
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
		[]string{tagCrabbox, "crabbox:lease:cbx_1", "crabbox:state:running", "other"},
		[]string{tagCrabbox, "crabbox:lease:cbx_1", "crabbox:state:ready"},
	)
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	for _, req := range requests {
		if req.Method == http.MethodDelete {
			deleted = append(deleted, req.Path)
		}
	}
	if len(deleted) != 1 || !strings.Contains(deleted[0], "crabbox:state:running") {
		t.Fatalf("deleted=%v requests=%v", deleted, requests)
	}
}

func TestDigitalOceanClientCreateDropletRollsBackNewSSHKeyOnTagFailure(t *testing.T) {
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
		case r.Method == http.MethodPost && r.URL.Path == "/tags":
			http.Error(w, "tag denied", http.StatusForbidden)
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
