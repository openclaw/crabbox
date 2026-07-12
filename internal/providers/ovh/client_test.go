package ovh

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientRefusesCrossOriginRedirectBeforeSignedHeaderReplay(t *testing.T) {
	var sinkRequests atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		sinkRequests.Add(1)
	}))
	defer sink.Close()

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, header := range []string{"X-Ovh-Application", "X-Ovh-Consumer", "X-Ovh-Timestamp", "X-Ovh-Signature"} {
			if r.Header.Get(header) == "" {
				t.Errorf("origin request missing %s", header)
			}
		}
		http.Redirect(w, r, sink.URL+"/stolen?location-secret=value#fragment-secret", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := newTestClient(t, origin.URL)
	_, err := client.ListProjects(context.Background())
	if !errors.Is(err, errOVHCrossOriginRedirect) {
		t.Fatalf("error=%v want cross-origin redirect refusal", err)
	}
	if got := sinkRequests.Load(); got != 0 {
		t.Fatalf("redirect sink received %d requests", got)
	}
	for _, leaked := range []string{"location-secret", "fragment-secret", "/stolen"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("redirect error leaked %q: %v", leaked, err)
		}
	}
}

func TestClientFollowsSameOriginRedirectWithSignedHeaders(t *testing.T) {
	var redirected atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cloud/project":
			http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
		case "/redirected":
			redirected.Store(true)
			for _, header := range []string{"X-Ovh-Application", "X-Ovh-Consumer", "X-Ovh-Timestamp", "X-Ovh-Signature"} {
				if r.Header.Get(header) == "" {
					t.Errorf("redirected request missing %s", header)
				}
			}
			_ = json.NewEncoder(w).Encode([]string{"project-test"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	projects, err := client.ListProjects(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !redirected.Load() || len(projects) != 1 || projects[0].Key() != "project-test" {
		t.Fatalf("redirected=%t projects=%v", redirected.Load(), projects)
	}
}

func TestClientPreservesCallerRedirectPolicy(t *testing.T) {
	wantErr := errors.New("caller stopped redirect")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	source := server.Client()
	source.CheckRedirect = func(*http.Request, []*http.Request) error { return wantErr }

	client := newTestClientWithHTTP(t, server.URL, source)
	_, err := client.ListProjects(context.Background())
	if !errors.Is(err, wantErr) {
		t.Fatalf("error=%v want caller redirect policy", err)
	}
}

func TestClientSanitizesRedirectLimit(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hop := requests.Add(1)
		http.Redirect(w, r, fmt.Sprintf("/redirect/%d?limit-secret=value#limit-fragment", hop), http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.ListProjects(context.Background())
	if !errors.Is(err, errOVHRedirectLimit) {
		t.Fatalf("error=%v want redirect limit", err)
	}
	for _, leaked := range []string{"limit-secret", "limit-fragment", "/redirect/"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("redirect limit error leaked %q: %v", leaked, err)
		}
	}
}

func TestClientSanitizesMalformedRedirect(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://redirect.example.test/%zz?location-secret=value")
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.ListProjects(context.Background())
	if !errors.Is(err, errOVHInvalidRedirect) {
		t.Fatalf("error=%v want invalid redirect refusal", err)
	}
	if strings.Contains(err.Error(), "location-secret") || strings.Contains(err.Error(), "%zz") {
		t.Fatalf("invalid redirect error leaked Location details: %v", err)
	}
}

func TestClientRedirectGuardUsesEffectiveOrigin(t *testing.T) {
	base, _ := url.Parse("https://api.ovh.example.test")
	same, _ := url.Parse("https://api.ovh.example.test:443/redirected")
	otherPort, _ := url.Parse("https://api.ovh.example.test:444/redirected")
	otherScheme, _ := url.Parse("http://api.ovh.example.test:443/redirected")
	if !sameOVHOrigin(base, same) {
		t.Fatal("default HTTPS port should share origin")
	}
	if sameOVHOrigin(base, otherPort) {
		t.Fatal("different effective port should be refused")
	}
	if sameOVHOrigin(base, otherScheme) {
		t.Fatal("different scheme should be refused")
	}
}

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
		_ = json.NewEncoder(w).Encode([]string{"GRA11"})
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

func TestClientSynchronizesServerTimeBeforeFirstSignedRequest(t *testing.T) {
	var paths []string
	var gotTimestamp string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/auth/time":
			_, _ = w.Write([]byte("2000"))
		case "/cloud/project":
			gotTimestamp = r.Header.Get("X-Ovh-Timestamp")
			_ = json.NewEncoder(w).Encode([]string{"project-test"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	client.hasServerTime = false
	client.now = func() int64 { return 1000 }
	if _, err := client.ListProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(paths, ",") != "/auth/time,/cloud/project" {
		t.Fatalf("paths=%v", paths)
	}
	if gotTimestamp != "2000" {
		t.Fatalf("timestamp=%q", gotTimestamp)
	}
}

func TestCloudPathPreservesEscapedDotSegments(t *testing.T) {
	cases := []struct {
		name      string
		projectID string
		parts     []string
		want      string
	}{
		{
			name:      "slash in project id",
			projectID: "project/id",
			parts:     []string{"region"},
			want:      "/cloud/project/project%2Fid/region",
		},
		{
			name:      "dot project id",
			projectID: ".",
			parts:     []string{"sshkey", ".."},
			want:      "/cloud/project/%2E/sshkey/%2E%2E",
		},
		{
			name:      "dot resource id",
			projectID: "project-test",
			parts:     []string{"instance", ".."},
			want:      "/cloud/project/project-test/instance/%2E%2E",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cloudPath(tc.projectID, tc.parts...); got != tc.want {
				t.Fatalf("cloudPath()=%q want %q", got, tc.want)
			}
		})
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

func TestClientSignsWithCachedOVHServerTime(t *testing.T) {
	var gotTimestamp, gotSignature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/time":
			_, _ = w.Write([]byte("2000"))
		case "/cloud/project":
			gotTimestamp = r.Header.Get("X-Ovh-Timestamp")
			gotSignature = r.Header.Get("X-Ovh-Signature")
			_ = json.NewEncoder(w).Encode([]string{"project-test"})
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer server.Close()

	localNow := int64(1000)
	client := newTestClient(t, server.URL)
	client.now = func() int64 { return localNow }
	if got, err := client.AuthTime(context.Background()); err != nil || got != 2000 {
		t.Fatalf("AuthTime()=%d err=%v", got, err)
	}
	localNow = 1005
	if _, err := client.ListProjects(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotTimestamp != "2005" {
		t.Fatalf("timestamp=%q want %q", gotTimestamp, "2005")
	}
	fullURL := server.URL + "/cloud/project"
	sum := sha1.Sum([]byte(strings.Join([]string{"app-secret", "consumer-key", "GET", fullURL, "", "2005"}, "+")))
	wantSignature := "$1$" + hex.EncodeToString(sum[:])
	if gotSignature != wantSignature {
		t.Fatalf("signature=%q want %q", gotSignature, wantSignature)
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
			_ = json.NewEncoder(w).Encode([]string{"project-test"})
		case "/cloud/project/project-test/region":
			_ = json.NewEncoder(w).Encode([]string{"BHS5", "GRA11"})
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
	projects, err := client.ListProjects(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(projects) != 1 || projects[0].Key() != "project-test" {
		t.Fatalf("projects=%#v", projects)
	}
	regions, err := client.ListRegions(ctx, "project-test")
	if err != nil {
		t.Fatal(err)
	}
	if len(regions) != 2 || regions[0].Name != "BHS5" || regions[1].Name != "GRA11" {
		t.Fatalf("regions=%#v", regions)
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

func TestInstanceDecodesTopLevelFlavorID(t *testing.T) {
	var instance Instance
	if err := json.Unmarshal([]byte(`{"id":"instance-id","flavorId":"b3-16"}`), &instance); err != nil {
		t.Fatal(err)
	}
	if instance.FlavorID != "b3-16" {
		t.Fatalf("instance=%#v", instance)
	}
}

func TestClientMutatingLifecycleMethods(t *testing.T) {
	var seen []string
	var instanceBody InstanceCreateRequest
	var keyBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.EscapedPath())
		switch r.Method + " " + r.URL.Path {
		case "POST /cloud/project/project-test/sshkey":
			if err := json.NewDecoder(r.Body).Decode(&keyBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(SSHKey{ID: "key-id", Name: keyBody["name"], PublicKey: keyBody["publicKey"]})
		case "DELETE /cloud/project/project-test/sshkey/key-id":
			w.WriteHeader(http.StatusNoContent)
		case "POST /cloud/project/project-test/instance":
			if err := json.NewDecoder(r.Body).Decode(&instanceBody); err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(Instance{ID: "instance-id", Name: instanceBody.Name, SSHKeyID: instanceBody.SSHKeyID})
		case "DELETE /cloud/project/project-test/instance/instance-id":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	key, err := client.CreateSSHKey(context.Background(), "project-test", "crabbox-cbx-test", "ssh-ed25519 test")
	if err != nil {
		t.Fatal(err)
	}
	if key.ID != "key-id" || keyBody["name"] != "crabbox-cbx-test" || keyBody["publicKey"] != "ssh-ed25519 test" {
		t.Fatalf("key=%#v body=%#v", key, keyBody)
	}
	created, err := client.CreateInstance(context.Background(), "project-test", InstanceCreateRequest{
		Name:     "cbx-test",
		Region:   "GRA11",
		FlavorID: "flavor-id",
		ImageID:  "image-id",
		SSHKeyID: key.ID,
		UserData: "#cloud-config",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != "instance-id" || instanceBody.FlavorID != "flavor-id" || instanceBody.ImageID != "image-id" || instanceBody.SSHKeyID != "key-id" || instanceBody.UserData != "#cloud-config" {
		t.Fatalf("created=%#v body=%#v", created, instanceBody)
	}
	if err := client.DeleteInstance(context.Background(), "project-test", "instance-id"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteSSHKey(context.Background(), "project-test", "key-id"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"POST /cloud/project/project-test/sshkey",
		"POST /cloud/project/project-test/instance",
		"DELETE /cloud/project/project-test/instance/instance-id",
		"DELETE /cloud/project/project-test/sshkey/key-id",
	}
	if strings.Join(seen, "\n") != strings.Join(want, "\n") {
		t.Fatalf("seen=%v want=%v", seen, want)
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

func TestClientErrorRedactsSecretsBeforeTruncating(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, strings.Repeat("x", 395)+"app-secret consumer-key", http.StatusForbidden)
	}))
	defer server.Close()

	client := newTestClient(t, server.URL)
	_, err := client.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, leaked := range []string{"app-", "app-secret", "consumer", "consumer-key"} {
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
	return newTestClientWithHTTP(t, endpoint, nil)
}

func newTestClientWithHTTP(t *testing.T, endpoint string, httpClient *http.Client) *Client {
	t.Helper()
	client, err := newClientWithConfig(clientConfig{
		Endpoint:          endpoint,
		ApplicationKey:    "app-key",
		ApplicationSecret: "app-secret",
		ConsumerKey:       "consumer-key",
		HTTP:              httpClient,
		AllowTestEndpoint: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	client.hasServerTime = true
	return client
}
