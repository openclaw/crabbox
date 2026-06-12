package ovh

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientSignsReadOnlyRequests(t *testing.T) {
	var gotMethod, gotPath, gotQuery, gotApplication, gotConsumer, gotTimestamp, gotSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotApplication = r.Header.Get("X-Ovh-Application")
		gotConsumer = r.Header.Get("X-Ovh-Consumer")
		gotTimestamp = r.Header.Get("X-Ovh-Timestamp")
		gotSignature = r.Header.Get("X-Ovh-Signature")
		_ = json.NewEncoder(w).Encode([]Region{{Name: "GRA11"}})
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	client.now = func() int64 { return 1234567890 }
	regions, err := client.ListRegions(context.Background(), "project/id")
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 || regions[0].Name != "GRA11" {
		t.Fatalf("regions=%#v", regions)
	}
	if gotMethod != http.MethodGet || gotPath != "/cloud/project/project%2Fid/region" || gotQuery != "" {
		t.Fatalf("request method=%s path=%s query=%s", gotMethod, gotPath, gotQuery)
	}
	if gotApplication != "app-key" || gotConsumer != "consumer-key" || gotTimestamp != "1234567890" {
		t.Fatalf("headers application=%q consumer=%q timestamp=%q", gotApplication, gotConsumer, gotTimestamp)
	}
	fullURL := server.URL + "/cloud/project/project%2Fid/region"
	sum := sha1.Sum([]byte(strings.Join([]string{"app-secret", "consumer-key", "GET", fullURL, "", "1234567890"}, "+")))
	wantSignature := "$1$" + hex.EncodeToString(sum[:])
	if gotSignature != wantSignature {
		t.Fatalf("signature=%q want %q", gotSignature, wantSignature)
	}
}

func TestClientAuthTimeIsUnsigned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/time" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		if r.Header.Get("X-Ovh-Consumer") != "" || r.Header.Get("X-Ovh-Signature") != "" {
			t.Fatalf("auth time should not be signed: headers=%v", r.Header)
		}
		_, _ = w.Write([]byte("1234567890"))
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	got, err := client.AuthTime(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != 1234567890 {
		t.Fatalf("timestamp=%d", got)
	}
}

func TestClientReadOnlyDiscoveryMethods(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected mutating method %s %s", r.Method, r.URL.String())
		}
		seen[r.URL.String()] = true
		switch r.URL.Path {
		case "/cloud/project":
			_ = json.NewEncoder(w).Encode([]Project{{ID: "project-test"}})
		case "/cloud/project/project-test/flavor":
			_ = json.NewEncoder(w).Encode([]Flavor{{ID: "flavor-id", Name: "b3-8"}})
		case "/cloud/project/project-test/flavor/flavor-id":
			_ = json.NewEncoder(w).Encode(Flavor{ID: "flavor-id", Name: "b3-8"})
		case "/cloud/project/project-test/image":
			_ = json.NewEncoder(w).Encode([]Image{{ID: "image-id", Name: "Ubuntu 24.04"}})
		case "/cloud/project/project-test/image/image-id":
			_ = json.NewEncoder(w).Encode(Image{ID: "image-id", Name: "Ubuntu 24.04"})
		case "/cloud/project/project-test/sshkey":
			_ = json.NewEncoder(w).Encode([]SSHKey{{ID: "key-id", Name: "crabbox"}})
		case "/cloud/project/project-test/sshkey/key-id":
			_ = json.NewEncoder(w).Encode(SSHKey{ID: "key-id", Name: "crabbox"})
		case "/cloud/project/project-test/instance":
			_ = json.NewEncoder(w).Encode([]Instance{{ID: "instance-id", Name: "crabbox-test"}})
		case "/cloud/project/project-test/instance/instance-id":
			_ = json.NewEncoder(w).Encode(Instance{ID: "instance-id", Name: "crabbox-test"})
		default:
			t.Fatalf("unexpected path %s", r.URL.String())
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	ctx := context.Background()
	if _, err := client.ListProjects(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListFlavors(ctx, "project-test", "GRA11"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetFlavor(ctx, "project-test", "flavor-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListImages(ctx, "project-test", "GRA11"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetImage(ctx, "project-test", "image-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListSSHKeys(ctx, "project-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSSHKey(ctx, "project-test", "key-id"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListInstances(ctx, "project-test"); err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetInstance(ctx, "project-test", "instance-id"); err != nil {
		t.Fatal(err)
	}
	if !seen["/cloud/project/project-test/flavor?region=GRA11"] || !seen["/cloud/project/project-test/image?region=GRA11"] {
		t.Fatalf("region-scoped discovery paths not seen: %v", seen)
	}
}

func TestClientErrorRedactsSecrets(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "app-secret consumer-key $1$0123456789012345678901234567890123456789", http.StatusForbidden)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, leaked := range []string{"app-secret", "consumer-key", "$1$0123456789012345678901234567890123456789"} {
		if strings.Contains(msg, leaked) {
			t.Fatalf("error leaked %q: %s", leaked, msg)
		}
	}
}

func TestClientRejectsNonOVHEndpoint(t *testing.T) {
	_, err := newClientWithConfig(clientConfig{
		Endpoint:          "http://attacker.example.test/1.0",
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
	})
	if err == nil || !strings.Contains(err.Error(), "ovh endpoint must use https") {
		t.Fatalf("err=%v", err)
	}
}

func TestClientAcceptsOVHEndpointAliasesAndHosts(t *testing.T) {
	for _, endpoint := range []string{
		"ovh-us",
		"ovh-ca",
		"ovh-eu",
		"https://ca.api.ovh.com/1.0",
		"https://eu.api.ovh.com/1.0",
		"https://api.us.ovhcloud.com/1.0",
	} {
		t.Run(endpoint, func(t *testing.T) {
			if _, err := newClientWithConfig(clientConfig{
				Endpoint:          endpoint,
				ApplicationKey:    "app-key",
				ApplicationSecret: "app-secret",
				ConsumerKey:       "consumer-key",
			}); err != nil {
				t.Fatalf("newClientWithConfig(%q): %v", endpoint, err)
			}
		})
	}
}

func newTestClient(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := newClientWithConfig(clientConfig{
		Endpoint:          endpoint,
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
		AllowTestEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}
