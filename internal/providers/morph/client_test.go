package morph

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestMorphClientRedactsReflectedCredential(t *testing.T) {
	const secret = "morph-secret-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"message":"Bearer `+secret+` quota exceeded"}`)
	}))
	defer server.Close()

	client := &morphClient{apiURL: server.URL, apiKey: secret, httpClient: server.Client()}
	err := client.DeleteInstance(context.Background(), "inst_1")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "quota exceeded") {
		t.Fatalf("DeleteInstance error=%v, want redacted useful provider error", err)
	}
}

func TestMorphClientBootSnapshotUsesAPIBaseAndAuth(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/snapshot/snapshot_123/boot" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization=%q", got)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != "{}" {
			t.Fatalf("body=%q", string(body))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "inst_1",
			"status": "ready",
			"refs":   map[string]any{"snapshot_id": "snapshot_123"},
		})
	}))
	defer server.Close()

	apiURL, err := normalizeMorphAPIURL(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client := &morphClient{apiURL: apiURL, apiKey: "token", httpClient: server.Client()}
	instance, err := client.BootSnapshot(context.Background(), "snapshot_123", morphBootSnapshotRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if instance.ID != "inst_1" || instance.Refs.SnapshotID != "snapshot_123" {
		t.Fatalf("unexpected instance: %#v", instance)
	}
}

func TestMorphClientRefusesCrossOriginRedirectBeforeReplay(t *testing.T) {
	targetRequests := 0
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetRequests++
		t.Errorf("redirect target received %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
	}))
	defer target.Close()
	trusted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/stolen?token=redirect-secret", http.StatusTemporaryRedirect)
	}))
	defer trusted.Close()

	client, err := newMorphClient(
		Config{Morph: MorphConfig{APIKey: "test-key", APIURL: trusted.URL}},
		Runtime{HTTP: trusted.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.GetSnapshot(context.Background(), "snapshot_123")
	if err == nil || !strings.Contains(err.Error(), "refused cross-origin redirect") {
		t.Fatalf("GetSnapshot error = %v, want cross-origin refusal", err)
	}
	if strings.Contains(err.Error(), "redirect-secret") {
		t.Fatalf("GetSnapshot error leaked redirect query: %v", err)
	}
	if targetRequests != 0 {
		t.Fatalf("redirect target received %d requests, want 0", targetRequests)
	}
}

func TestMorphClientFollowsSameOriginRedirect(t *testing.T) {
	var redirectedAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/snapshot/snapshot_123":
			http.Redirect(w, r, "/api/redirected", http.StatusTemporaryRedirect)
		case "/api/redirected":
			redirectedAuth = r.Header.Get("Authorization")
			_, _ = io.WriteString(w, `{"id":"snapshot_123"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := newMorphClient(
		Config{Morph: MorphConfig{APIKey: "test-key", APIURL: server.URL}},
		Runtime{HTTP: server.Client()},
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := client.GetSnapshot(context.Background(), "snapshot_123")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.ID != "snapshot_123" || redirectedAuth != "Bearer test-key" {
		t.Fatalf("snapshot=%#v auth=%q", snapshot, redirectedAuth)
	}
}

func TestMorphClientPreservesCallerRedirectPolicy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/redirected", http.StatusFound)
	}))
	defer server.Close()
	callerErr := errors.New("caller refused redirect")
	callerChecks := 0
	httpClient := server.Client()
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		callerChecks++
		return callerErr
	}
	client, err := newMorphClient(
		Config{Morph: MorphConfig{APIKey: "test-key", APIURL: server.URL}},
		Runtime{HTTP: httpClient},
	)
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.GetSnapshot(context.Background(), "snapshot_123")
	if !errors.Is(err, callerErr) || callerChecks != 1 {
		t.Fatalf("GetSnapshot error = %v, caller checks = %d", err, callerChecks)
	}
}

func TestSameMorphOrigin(t *testing.T) {
	trusted, _ := url.Parse("https://api.example.com:443/v1")
	for _, test := range []struct {
		name string
		raw  string
		want bool
	}{
		{name: "default https port", raw: "https://api.example.com/next", want: true},
		{name: "case insensitive host", raw: "https://API.EXAMPLE.COM/next", want: true},
		{name: "scheme drift", raw: "http://api.example.com/next"},
		{name: "subdomain drift", raw: "https://redirect.api.example.com/next"},
		{name: "port drift", raw: "https://api.example.com:444/next"},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate, _ := url.Parse(test.raw)
			if got := sameMorphOrigin(trusted, candidate); got != test.want {
				t.Fatalf("sameMorphOrigin(%q)=%v, want %v", test.raw, got, test.want)
			}
		})
	}
}

func TestMorphClientListInstancesAndWakeOnRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch requests {
		case 1:
			if r.Method != http.MethodGet || r.URL.Path != "/api/instance" {
				t.Fatalf("unexpected list request %s %s", r.Method, r.URL.Path)
			}
			if got := r.URL.Query().Get("metadata[crabbox]"); got != "true" {
				t.Fatalf("metadata[crabbox]=%q", got)
			}
			if got := r.URL.Query().Get("metadata[provider]"); got != providerName {
				t.Fatalf("metadata[provider]=%q", got)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]any{{
					"id":       "inst_1",
					"status":   "ready",
					"metadata": map[string]any{"crabbox": "true", "provider": providerName},
				}},
			})
		case 2:
			if r.Method != http.MethodPost || r.URL.Path != "/api/instance/inst_1/wake-on" {
				t.Fatalf("unexpected wake-on request %s %s", r.Method, r.URL.Path)
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["wake_on_ssh"] != true {
				t.Fatalf("wake_on_ssh=%v", body["wake_on_ssh"])
			}
			if _, ok := body["wake_on_http"]; ok {
				t.Fatalf("wake_on_http should be omitted: %#v", body)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request count %d", requests)
		}
	}))
	defer server.Close()

	client := &morphClient{apiURL: server.URL + "/api", apiKey: "token", httpClient: server.Client()}
	instances, err := client.ListInstances(context.Background(), map[string]string{"crabbox": "true", "provider": providerName})
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 1 || instances[0].ID != "inst_1" {
		t.Fatalf("unexpected instances: %#v", instances)
	}
	if err := client.UpdateInstanceWakeOn(context.Background(), "inst_1", boolPtr(true), nil); err != nil {
		t.Fatal(err)
	}
}
