package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// TestServerUnmarshalsHetznerPrivateNetArray locks in the JSON shape that the
// Hetzner Cloud API returns for the `private_net` field on a server. Hetzner
// documents this as an array of attachments (one entry per attached private
// network, empty when none — see
// https://docs.hetzner.cloud/#servers-get-all-servers).
//
// Before the privateNet UnmarshalJSON, Server.PrivateNet was a struct, so
// every call into ListCrabboxServers failed the moment Hetzner returned a
// server with `"private_net": []`, breaking `crabbox list`, `crabbox doctor`,
// `crabbox warmup`, and `crabbox run --id ...` for any Hetzner account with
// at least one server.
func TestServerUnmarshalsHetznerPrivateNetEmptyArray(t *testing.T) {
	payload := []byte(`{
        "servers": [
            {
                "id": 130281951,
                "name": "crabbox-swift-barnacle-6ee103fb",
                "status": "running",
                "labels": {"crabbox": "true"},
                "public_net": {"ipv4": {"ip": "91.99.223.29"}},
                "private_net": [],
                "server_type": {"name": "cpx62", "architecture": "x86"},
                "location": {"name": "fsn1"},
                "image": {"architecture": "x86"}
            }
        ]
    }`)

	var res struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("unmarshal Hetzner server list with empty private_net array failed: %v", err)
	}
	if len(res.Servers) != 1 {
		t.Fatalf("expected 1 server, got %d", len(res.Servers))
	}
	s := res.Servers[0]
	if s.ID != 130281951 {
		t.Errorf("ID: got %d, want 130281951", s.ID)
	}
	if s.PublicNet.IPv4.IP != "91.99.223.29" {
		t.Errorf("PublicNet.IPv4.IP: got %q, want %q", s.PublicNet.IPv4.IP, "91.99.223.29")
	}
	if got := s.PrivateNet.IPv4.IP; got != "" {
		t.Errorf("PrivateNet.IPv4.IP: got %q, want empty (no attachments)", got)
	}
	if s.ServerType.Name != "cpx62" {
		t.Errorf("ServerType.Name: got %q, want %q", s.ServerType.Name, "cpx62")
	}
	if s.Location == nil || s.Image == nil || s.Location.Name != "fsn1" || s.ServerType.Architecture != "x86" || s.Image.Architecture != "x86" {
		t.Errorf("source metadata: location=%+v server_arch=%q image=%+v", s.Location, s.ServerType.Architecture, s.Image)
	}
}

// TestServerUnmarshalsHetznerPrivateNetAttached verifies the best-effort
// behaviour: when Hetzner returns at least one attachment, the first one's
// `ip` lands in PrivateNet.IPv4.IP so any caller that wants a private IP for
// a Hetzner-leased box still sees one.
func TestServerUnmarshalsHetznerPrivateNetAttached(t *testing.T) {
	payload := []byte(`{
        "servers": [
            {
                "id": 1,
                "name": "attached",
                "status": "running",
                "public_net": {"ipv4": {"ip": "1.2.3.4"}},
                "private_net": [
                    {"network": 42, "ip": "10.0.0.5", "alias_ips": [], "mac_address": "86:00:00:00:00:01"},
                    {"network": 43, "ip": "10.1.0.7", "alias_ips": [], "mac_address": "86:00:00:00:00:02"}
                ],
                "server_type": {"name": "cpx11"}
            }
        ]
    }`)

	var res struct {
		Servers []Server `json:"servers"`
	}
	if err := json.Unmarshal(payload, &res); err != nil {
		t.Fatalf("unmarshal Hetzner server list with attached private_net failed: %v", err)
	}
	if got, want := res.Servers[0].PrivateNet.IPv4.IP, "10.0.0.5"; got != want {
		t.Errorf("PrivateNet.IPv4.IP: got %q, want %q (first attachment)", got, want)
	}
}

// TestServerUnmarshalsLegacyPrivateNetStruct confirms the legacy
// `{"ipv4": {"ip": "..."}}` struct shape still unmarshals — covers anything
// that round-trips a Server through JSON outside the Hetzner API (test
// fixtures, snapshots, golden files).
func TestServerUnmarshalsLegacyPrivateNetStruct(t *testing.T) {
	payload := []byte(`{
        "id": 7,
        "name": "legacy",
        "private_net": {"ipv4": {"ip": "10.42.0.4"}}
    }`)

	var s Server
	if err := json.Unmarshal(payload, &s); err != nil {
		t.Fatalf("unmarshal legacy private_net struct shape failed: %v", err)
	}
	if got, want := s.PrivateNet.IPv4.IP, "10.42.0.4"; got != want {
		t.Errorf("PrivateNet.IPv4.IP: got %q, want %q", got, want)
	}
}

// TestServerUnmarshalsPrivateNetNullAndOmitted documents the zero-value
// behaviour for null or missing `private_net` — important because some
// future schema change or non-Hetzner caller could omit the field.
func TestServerUnmarshalsPrivateNetNullAndOmitted(t *testing.T) {
	for name, payload := range map[string][]byte{
		"null":    []byte(`{"id": 1, "private_net": null}`),
		"omitted": []byte(`{"id": 1}`),
	} {
		t.Run(name, func(t *testing.T) {
			var s Server
			if err := json.Unmarshal(payload, &s); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if s.PrivateNet.IPv4.IP != "" {
				t.Errorf("expected empty IP, got %q", s.PrivateNet.IPv4.IP)
			}
		})
	}
}

func TestHetznerImageLifecycleRequests(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization=%q", got)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/servers/42/actions/create_image":
			var body struct {
				Type        string            `json:"type"`
				Description string            `json:"description"`
				Labels      map[string]string `json:"labels"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode request: %v", err)
				return
			}
			if body.Type != "snapshot" || body.Description != "checkpoint" || body.Labels["checkpoint"] != "chk_123" {
				t.Errorf("body=%+v", body)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"image":{"id":99,"type":"snapshot","status":"creating","description":"checkpoint","architecture":"x86","labels":{"checkpoint":"chk_123"}}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/images/99":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"image":{"id":99,"type":"snapshot","status":"available","architecture":"x86"}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/images/99":
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &HetznerClient{Token: "test-token", Client: server.Client(), BaseURL: server.URL}
	created, err := client.CreateServerSnapshot(context.Background(), 42, "checkpoint", map[string]string{"checkpoint": "chk_123"})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != 99 || created.Type != "snapshot" || created.Status != "creating" || created.Architecture != "x86" {
		t.Fatalf("created=%+v", created)
	}
	got, err := client.GetImage(context.Background(), 99)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "available" {
		t.Fatalf("image=%+v", got)
	}
	if err := client.DeleteImage(context.Background(), 99); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /servers/42/actions/create_image", "GET /images/99", "DELETE /images/99"}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%v, want %v", requests, want)
	}
}

func TestHetznerNotFoundRecognitionIsTypedAndExact(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"code":"not_found"}}`, http.StatusNotFound)
	}))
	defer server.Close()
	client := &HetznerClient{Token: "test-token", Client: server.Client(), BaseURL: server.URL}
	_, err := client.GetImage(context.Background(), 99)
	if err == nil || !IsHetznerNotFound(err) {
		t.Fatalf("err=%v, want typed not found", err)
	}
	var httpErr HetznerHTTPError
	if !errors.As(err, &httpErr) || httpErr.Method != http.MethodGet || httpErr.Path != "/images/99" || httpErr.StatusCode != http.StatusNotFound {
		t.Fatalf("http error=%+v", httpErr)
	}
	if strings.Contains(err.Error(), "test-token") {
		t.Fatal("error exposed bearer token")
	}
	if IsHetznerNotFound(errors.New("hetzner GET /images/99: http 404: fake")) {
		t.Fatal("string-shaped error was accepted as an exact 404")
	}
}
