package morph

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
