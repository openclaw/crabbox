package cli

import (
	"encoding/json"
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
                "server_type": {"name": "cpx62"}
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
