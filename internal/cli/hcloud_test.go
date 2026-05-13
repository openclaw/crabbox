package cli

import (
	"encoding/json"
	"testing"
)

func TestServerUnmarshalsHetznerPrivateNetArray(t *testing.T) {
	data := []byte(`{
		"id": 123,
		"name": "crabbox-blue-lobster",
		"status": "running",
		"labels": {"crabbox": "true"},
		"public_net": {"ipv4": {"ip": "203.0.113.10"}},
		"private_net": [
			{"ipv4": {"ip": "10.0.0.5"}}
		],
		"server_type": {"name": "cpx22"}
	}`)

	var server Server
	if err := json.Unmarshal(data, &server); err != nil {
		t.Fatalf("unmarshal server: %v", err)
	}
	if server.PrivateNet.IPv4.IP != "10.0.0.5" {
		t.Fatalf("private IP: got %q", server.PrivateNet.IPv4.IP)
	}
}

func TestServerUnmarshalsObjectPrivateNet(t *testing.T) {
	data := []byte(`{
		"id": 123,
		"private_net": {"ipv4": {"ip": "10.0.0.6"}}
	}`)

	var server Server
	if err := json.Unmarshal(data, &server); err != nil {
		t.Fatalf("unmarshal server: %v", err)
	}
	if server.PrivateNet.IPv4.IP != "10.0.0.6" {
		t.Fatalf("private IP: got %q", server.PrivateNet.IPv4.IP)
	}
}
