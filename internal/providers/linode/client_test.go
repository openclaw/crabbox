package linode

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

func TestLinodeClientCreateRequestShape(t *testing.T) {
	var captured struct {
		Method      string
		Path        string
		Auth        string
		Accept      string
		ContentType string
		Body        map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.RequestURI()
		captured.Auth = r.Header.Get("Authorization")
		captured.Accept = r.Header.Get("Accept")
		captured.ContentType = r.Header.Get("Content-Type")
		if err := json.NewDecoder(r.Body).Decode(&captured.Body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/linode/instances" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"id":123,"label":"crabbox-cbx-test","status":"provisioning","region":"us-ord","type":"g6-standard-1","image":"linode/ubuntu24.04","tags":["crabbox"]}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "secret-token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	got, err := client.CreateLinode(context.Background(), createLinodeRequest{
		Region:         "us-ord",
		Type:           "g6-standard-1",
		Image:          "linode/ubuntu24.04",
		Label:          "crabbox-cbx-test",
		Tags:           []string{"crabbox", "lease:cbx_test"},
		AuthorizedKeys: []string{"ssh-ed25519 test"},
		Metadata:       &linodeMetadata{UserData: "I2Nsb3VkLWNvbmZpZw=="},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != 123 || got.Label != "crabbox-cbx-test" {
		t.Fatalf("instance=%#v", got)
	}
	if captured.Method != http.MethodPost || captured.Path != "/linode/instances" {
		t.Fatalf("request=%s %s", captured.Method, captured.Path)
	}
	if captured.Auth != "Bearer secret-token" || captured.Accept != "application/json" || !strings.HasPrefix(captured.ContentType, "application/json") {
		t.Fatalf("headers auth=%q accept=%q content-type=%q", captured.Auth, captured.Accept, captured.ContentType)
	}
	if captured.Body["region"] != "us-ord" || captured.Body["type"] != "g6-standard-1" || captured.Body["image"] != "linode/ubuntu24.04" {
		t.Fatalf("body=%v", captured.Body)
	}
	keys, _ := captured.Body["authorized_keys"].([]any)
	if len(keys) != 1 || keys[0] != "ssh-ed25519 test" {
		t.Fatalf("authorized_keys=%v", captured.Body["authorized_keys"])
	}
	metadata, _ := captured.Body["metadata"].(map[string]any)
	if metadata["user_data"] != "I2Nsb3VkLWNvbmZpZw==" {
		t.Fatalf("metadata=%v", captured.Body["metadata"])
	}
}

func TestLinodeClientAccountID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/account" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"euuid":"A1BC2DEF-34GH-567I-J890KLMN12O34P56"}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	accountID, err := client.AccountID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if accountID != "euuid:A1BC2DEF-34GH-567I-J890KLMN12O34P56" {
		t.Fatalf("accountID=%q", accountID)
	}
}

func TestLinodeClientAccountSettings(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/account/settings" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		_, _ = w.Write([]byte(`{"interfaces_for_new_linodes":"linode_only"}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	settings, err := client.AccountSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if settings.InterfacesForNewLinodes != "linode_only" {
		t.Fatalf("settings=%#v", settings)
	}
}

func TestLinodeClientUpdateTagsRequestShape(t *testing.T) {
	var captured struct {
		Method string
		Path   string
		Body   map[string]any
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured.Method = r.Method
		captured.Path = r.URL.RequestURI()
		if err := json.NewDecoder(r.Body).Decode(&captured.Body); err != nil {
			t.Fatal(err)
		}
		if r.Method != http.MethodPut || r.URL.Path != "/linode/instances/123" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.RequestURI())
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	if err := client.UpdateLinodeTags(context.Background(), 123, []string{"crabbox", "crabbox:state:ready"}); err != nil {
		t.Fatal(err)
	}
	tags, _ := captured.Body["tags"].([]any)
	if captured.Method != http.MethodPut || captured.Path != "/linode/instances/123" || len(tags) != 2 || tags[1] != "crabbox:state:ready" {
		t.Fatalf("captured=%#v", captured)
	}
}

func TestLinodeClientPagination(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.RequestURI())
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(`{"data":[{"id":1,"label":"one"}],"page":1,"pages":2,"results":2}`))
		case "2":
			_, _ = w.Write([]byte(`{"data":[{"id":2,"label":"two"}],"page":2,"pages":2,"results":2}`))
		default:
			t.Fatalf("unexpected page path %s", r.URL.RequestURI())
		}
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	linodes, err := client.ListLinodes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(linodes) != 2 || linodes[0].ID != 1 || linodes[1].ID != 2 {
		t.Fatalf("linodes=%v", linodes)
	}
	if strings.Join(paths, ",") != "/linode/instances?page=1&page_size=500,/linode/instances?page=2&page_size=500" {
		t.Fatalf("paths=%v", paths)
	}
}

func TestLinodeClientErrorRedaction(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errors":[{"reason":"token secret-token root_pass hunter2 user_data I2Nsb3VkLWNvbmZpZw=="}],"root_pass":"hunter2","user_data":"I2Nsb3VkLWNvbmZpZw=="}`, http.StatusBadRequest)
	}))
	defer server.Close()

	t.Setenv(tokenEnv, "secret-token")
	client, err := newLinodeClient(core.Runtime{HTTP: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	client.baseURL = server.URL
	err = client.do(context.Background(), http.MethodPost, "/linode/instances", createLinodeRequest{}, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	text := err.Error()
	for _, secret := range []string{"secret-token", "hunter2", "I2Nsb3VkLWNvbmZpZw=="} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked %q: %s", secret, text)
		}
	}
	if !strings.Contains(text, "<redacted>") {
		t.Fatalf("error not redacted: %s", text)
	}
}
