package asciibox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFixedBoxKeyedAPIWire(t *testing.T) {
	for _, org := range []string{"personal", "team_fixture"} {
		t.Run(org, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.URL.Path != "/api/box/v1/boxes" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer synthetic-fixture" || r.Header.Get("Idempotency-Key") != "crabbox-fixture-key" {
					t.Errorf("wrong native API routing or headers")
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["ttlSeconds"] != float64(3600) {
					t.Errorf("wrong native TTL: %v", body)
				}
				if org == "personal" && body["org"] != nil || org != "personal" && body["org"] != org {
					t.Errorf("wrong org scope: %v", body)
				}
				w.WriteHeader(http.StatusAccepted)
				io.WriteString(w, `{"ok":true,"type":"box.created","box":{"id":"bx_native","createdAt":"2026-09-20T12:00:00Z","state":"provisioning"}}`)
			}))
			defer server.Close()
			c := &client{apiURL: server.URL, apiKey: "synthetic-fixture", org: org, http: server.Client()}
			box, err := c.CreateBox(t.Context(), createRequest{TTL: time.Hour, IdempotencyKey: "crabbox-fixture-key"})
			if err != nil || box.ID != "bx_native" || box.createdID != box.ID || calls != 1 {
				t.Fatalf("create: %+v %v calls=%d", box, err, calls)
			}
		})
	}
}

func TestFixedBoxKeyedAPIRejectsUncertainResponses(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
	}{
		{"lost-body", 202, `{"box":`},
		{"duplicate-id", 202, `{"ok":true,"type":"box.created","box":{"id":"bx_one","id":"bx_two"}}`},
		{"wrong-envelope", 202, `{"ok":false,"box":{"id":"bx_one"}}`},
		{"ambiguous-envelope", 202, `{"ok":true,"type":"box.created","box":{"id":"bx_one"},"sandbox":{"id":"bx_two"}}`},
		{"server-error", 503, "synthetic-do-not-print"},
		{"redirect", 307, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Location", "/second-create")
				w.WriteHeader(test.code)
				io.WriteString(w, test.body)
			}))
			defer server.Close()
			c := &client{apiURL: server.URL, apiKey: "synthetic-fixture", http: server.Client()}
			box, err := c.CreateBox(t.Context(), createRequest{IdempotencyKey: "key"})
			if err == nil || box.ID != "" || calls != 1 || strings.Contains(err.Error(), "synthetic-do-not-print") {
				t.Fatalf("uncertain evidence accepted or resubmitted: %+v %v calls=%d", box, err, calls)
			}
		})
	}
}
