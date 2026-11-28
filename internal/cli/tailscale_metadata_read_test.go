package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailscaleMetadataReadScriptKeepsFieldsAligned(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("sh not available")
	}
	for _, tc := range []struct {
		name    string
		files   map[string]string
		want    TailscaleMetadata
		wantErr bool
	}{
		{
			name: "newline-terminated files as bootstrap writes them",
			files: map[string]string{
				"tailscale-ipv4":                       "100.64.0.7\n",
				"tailscale-hostname":                   "crabbox-blue-lobster\n",
				"tailscale-fqdn":                       "crabbox-blue-lobster.example.ts.net.\n",
				"tailscale-exit-node":                  "exit-1\n",
				"tailscale-exit-node-allow-lan-access": "true\n",
				"tailscale-version":                    "1.90.2\n",
				"tailscale-device-id":                  "nDEV123\n",
			},
			want: TailscaleMetadata{
				Enabled: true, State: "ready", IPv4: "100.64.0.7",
				Hostname: "crabbox-blue-lobster", FQDN: "crabbox-blue-lobster.example.ts.net.",
				ExitNode: "exit-1", ExitNodeAllowLANAccess: true, Version: "1.90.2", DeviceID: "nDEV123",
			},
		},
		{
			name: "absent and empty optional files keep their slot",
			files: map[string]string{
				"tailscale-ipv4":      "100.64.0.8\n",
				"tailscale-hostname":  "crabbox-red-crab\n",
				"tailscale-fqdn":      "",
				"tailscale-version":   "1.90.2",
				"tailscale-device-id": "nDEV456\n",
			},
			want: TailscaleMetadata{
				Enabled: true, State: "ready", IPv4: "100.64.0.8",
				Hostname: "crabbox-red-crab", Version: "1.90.2", DeviceID: "nDEV456",
			},
		},
		{
			name: "missing ipv4 is not filled from the next file",
			files: map[string]string{
				"tailscale-hostname": "100.64.0.9-looking-host\n",
				"tailscale-fqdn":     "crabbox-green-krill.example.ts.net.\n",
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			out, err := exec.Command(sh, "-c", tailscaleMetadataReadScript(dir)).Output()
			if err != nil {
				t.Fatalf("read script: %v", err)
			}
			// runSSHOutput hands the parser whitespace-trimmed output.
			got, err := parseRemoteTailscaleMetadata(strings.TrimSpace(string(out)))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected missing ipv4 error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got.IPv4 != tc.want.IPv4 || got.Hostname != tc.want.Hostname || got.FQDN != tc.want.FQDN ||
				got.ExitNode != tc.want.ExitNode || got.ExitNodeAllowLANAccess != tc.want.ExitNodeAllowLANAccess ||
				got.Version != tc.want.Version || got.DeviceID != tc.want.DeviceID ||
				got.Enabled != tc.want.Enabled || got.State != tc.want.State {
				t.Fatalf("metadata = %+v, want %+v", got, tc.want)
			}
		})
	}
}
