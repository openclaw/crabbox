package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	_ "github.com/openclaw/crabbox/internal/providers/hetzner"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestPondPeersUsesAWSLeaseTransport(t *testing.T) {
	for _, tc := range []struct {
		name      string
		labels    map[string]string
		sshHost   string
		transport string
		endpoint  string
	}{
		{"tailnet IPv4", map[string]string{"tailscale_ipv4": "100.64.1.3"}, "203.0.113.10", core.TransportTailnet, "100.64.1.3"},
		{"tailnet FQDN", map[string]string{"tailscale_fqdn": "web.example.ts.net"}, "203.0.113.10", core.TransportTailnet, "web.example.ts.net"},
		{"SSH without enrollment", nil, "203.0.113.10", core.TransportSSH, "ssh://203.0.113.10:22"},
		{"pending enrollment", map[string]string{"tailscale": "true", "tailscale_state": "requested"}, "203.0.113.10", core.TransportPending, ""},
		{"pending endpoint", nil, "", core.TransportPending, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seedAWSPondClaim(t, tc.labels, tc.sshHost)
			var stdout, stderr bytes.Buffer
			if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"pond", "peers", "--pond", "alpha", "--json"}); err != nil {
				t.Fatalf("pond peers: %v; stderr=%s", err, &stderr)
			}
			var result struct {
				Members []core.BridgePeer `json:"members"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Members) != 1 {
				t.Fatalf("members=%#v", result.Members)
			}
			peer := result.Members[0]
			if peer.Provider != "aws" || peer.Transport != tc.transport || peer.Endpoint != tc.endpoint {
				t.Fatalf("peer=%#v; want transport=%s endpoint=%s", peer, tc.transport, tc.endpoint)
			}
			if !reflect.DeepEqual(peer.Transports, []string{core.TransportTailnet, core.TransportSSH}) {
				t.Fatalf("transports=%v; want tailnet and ssh", peer.Transports)
			}
		})
	}
}

func TestPondDoctorRecognizesAWSTailnetEnrollment(t *testing.T) {
	seedAWSPondClaim(t, map[string]string{"tailscale_ipv4": "100.64.1.3"}, "203.0.113.10")
	var stdout, stderr bytes.Buffer
	if err := (core.App{Stdout: &stdout, Stderr: &stderr}).Run(context.Background(), []string{"doctor", "--pond", "alpha", "--json"}); err != nil {
		t.Fatalf("doctor: %v; stderr=%s; stdout=%s", err, &stderr, &stdout)
	}
	var result struct {
		Checks []struct {
			Check   string            `json:"check"`
			Details map[string]string `json:"details"`
		} `json:"checks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	for _, check := range result.Checks {
		if check.Check == "pond" {
			if check.Details["reason"] != "ts_api_key_missing" {
				t.Fatalf("enrolled AWS pond skipped before ACL verification: %#v", check)
			}
			return
		}
	}
	t.Fatal("doctor omitted the pond check")
}

func seedAWSPondClaim(t *testing.T, labels map[string]string, sshHost string) {
	t.Helper()
	dirs := testutil.IsolateUserDirs(t)
	t.Chdir(dirs.Root)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(dirs.ConfigHome, "crabbox.yaml"))
	for _, key := range []string{"CRABBOX_PROVIDER", "CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_TOKEN", "CRABBOX_COORDINATOR_TOKEN_COMMAND", "CRABBOX_BROKER_PROVIDER", "TS_API_KEY"} {
		t.Setenv(key, "")
	}
	t.Setenv("CRABBOX_OWNER", "alice@example.com")
	server := core.Server{Provider: "aws", CloudID: "i-test", Labels: labels}
	target := core.SSHTarget{Host: sshHost, Port: "22", TargetOS: core.TargetLinux}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_web", "web", core.Config{Provider: "aws", Pond: "alpha"}, server, target, dirs.Root, time.Hour, false); err != nil {
		t.Fatal(err)
	}
}
