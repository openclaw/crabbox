package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/api/option"
)

func TestGCPTailscaleMetadataRoundTrip(t *testing.T) {
	for _, fqdn := range []string{"crabbox-blue.tail1234.ts.net.", strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + ".tail1234.ts.net."} {
		t.Run(fqdn, func(t *testing.T) {
			stored := map[string]string{}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/setLabels") {
					var body struct{ Labels map[string]string }
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					stored = body.Labels
					valid := regexp.MustCompile(`^[a-z0-9_-]{0,63}$`)
					for k, v := range stored {
						if !valid.MatchString(v) || len(k) > 63 {
							t.Errorf("invalid GCP label %s=%q", k, v)
						}
					}
					if len(stored) > 64 {
						t.Errorf("too many labels: %d", len(stored))
					}
					fmt.Fprint(w, `{"name":"op","status":"DONE"}`)
				} else if strings.Contains(r.URL.Path, "/operations/") {
					fmt.Fprint(w, `{"name":"op","status":"DONE"}`)
				} else {
					json.NewEncoder(w).Encode(map[string]any{"id": "1", "name": "crabbox-blue", "labels": stored, "labelFingerprint": "x"})
				}
			}))
			defer api.Close()
			cfg := baseConfig()
			cfg.GCP = GCPConfig{Project: "p", Zone: "z"}
			client, err := newGCPClientWithOptions(context.Background(), cfg, option.WithoutAuthentication(), option.WithEndpoint(api.URL))
			if err != nil {
				t.Fatal(err)
			}
			want := map[string]string{"tailscale": "true", "tailscale_ipv4": "100.64.0.5", "tailscale_fqdn": fqdn, "tailscale_exit_node": "100.100.100.100", "tailscale_version": "1.104.2", "tailscale_device_id": "nodeABC123", "ttl_secs": "3600"}
			for i := 0; i < 2; i++ {
				if err := client.SetLabels(context.Background(), "crabbox-blue", want); err != nil {
					t.Fatal(err)
				}
				got, err := client.GetServer(context.Background(), "crabbox-blue")
				if err != nil {
					t.Fatal(err)
				}
				for k, v := range want {
					if got.Labels[k] != v {
						t.Errorf("round %d: %s=%q want %q", i, k, got.Labels[k], v)
					}
				}
				want = got.Labels
			}
		})
	}
}

func TestHetznerTailscaleDiagnosticsCannotBlockTouch(t *testing.T) {
	var stored map[string]string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Labels map[string]string }
		json.NewDecoder(r.Body).Decode(&body)
		valid := regexp.MustCompile(`^(([a-z0-9A-Z](?:[\-_.]|[a-z0-9A-Z]){0,61})?[a-z0-9A-Z]$|$)`)
		for k, v := range body.Labels {
			if !valid.MatchString(v) {
				http.Error(w, "invalid label "+k, 422)
				return
			}
		}
		stored = body.Labels
		fmt.Fprint(w, `{}`)
	}))
	defer api.Close()
	client := HetznerClient{Token: "fixture", Client: api.Client(), BaseURL: api.URL}
	labels := map[string]string{"state": "running", "expires_at": "1793882100", "tailscale_error": strings.Repeat("ssh: connect refused! ", 10), "tailscale_fqdn": "crabbox-blue.tail1234.ts.net.", "custom": "Keep_Me"}
	original := cloneStringMap(labels)
	if err := client.SetLabels(context.Background(), 42, labels); err != nil {
		t.Fatal(err)
	}
	if stored["state"] != "running" || stored["expires_at"] != labels["expires_at"] || stored["custom"] != "Keep_Me" {
		t.Fatalf("lost core metadata: %v", stored)
	}
	if !reflect.DeepEqual(labels, original) {
		t.Fatal("mutated source labels")
	}
}

func TestSuccessfulTailscaleMetadataClearsPreviousError(t *testing.T) {
	server := Server{Labels: map[string]string{"tailscale": "true", "tailscale_error": "previous read failed"}}
	applyTailscaleMetadataToServer(&server, TailscaleMetadata{Enabled: true, State: "ready", IPv4: "100.64.0.5"})
	if _, exists := server.Labels["tailscale_error"]; exists {
		t.Fatal("successful metadata still carries the previous failure")
	}
}
