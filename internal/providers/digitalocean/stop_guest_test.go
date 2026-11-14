package digitalocean

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestDigitalOceanStopPreparesLiveGuest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("recording SSH fixture requires /bin/sh")
	}
	for _, mode := range []string{"live", "live by claim", "ssh failure", "invalid key namespace", "absent", "ownership mismatch"} {
		t.Run(mode, func(t *testing.T) {
			testutil.IsolateUserDirs(t)
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("CRABBOX_CONFIG", filepath.Join(dir, "missing.yaml"))
			for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_TOKEN", "CRABBOX_PROVIDER", "CRABBOX_SSH_KEY"} {
				t.Setenv(name, "")
			}
			t.Setenv("DIGITALOCEAN_TOKEN", "synthetic-token")
			t.Setenv("CRABBOX_NETWORK", "public")
			const id = "cbx_abcdef123456"
			cfg := core.BaseConfig()
			cfg.Provider = providerName
			cfg.Tailscale.Enabled = true
			labels := core.DirectLeaseLabels(cfg, id, "stop-guest", providerName, "", false, time.Now())
			labels[digitalOceanAccountLabel] = "team:test-account"
			item := droplet{ID: 77, Name: core.LeaseProviderName(id, "stop-guest"), Status: "active", Tags: tagsFromLabels(labels)}
			item.Networks.V4 = append(item.Networks.V4, struct {
				IPAddress string `json:"ip_address"`
				Type      string `json:"type"`
			}{"192.0.2.77", "public"})
			server := serverFromDroplet(item, cfg)
			server.Labels[digitalOceanAccountLabel] = "team:test-account"
			server.Labels[digitalOceanKeyOwnedLabel] = "false"
			if err := core.ClaimLeaseTargetForRepoConfig(id, "stop-guest", cfg, server, core.SSHTarget{}, dir, time.Hour, false); err != nil {
				t.Fatal(err)
			}
			key := writeStoredTestboxKey(t, id)
			if mode == "invalid key namespace" {
				if err := os.Remove(key); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(key + ".pub"); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Dir(key)); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Dir(key), []byte("not a directory"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "ownership mismatch" {
				labels["lease"] = "cbx_123456abcdef"
				item.Tags = tagsFromLabels(labels)
			}
			deleted := 0
			original := http.DefaultTransport
			http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
				code := 200
				var data any
				switch {
				case req.Method == "GET" && req.URL.Path == "/v2/account":
					data = map[string]any{"account": map[string]any{"team": map[string]string{"uuid": "test-account"}}}
				case req.Method == "GET" && req.URL.Path == "/v2/droplets":
					items := []droplet{item}
					if mode == "absent" || mode == "live by claim" {
						items = nil
					}
					data = map[string]any{"droplets": items}
				case req.Method == "GET" && req.URL.Path == "/v2/droplets/77":
					if mode == "absent" {
						code = 404
						data = map[string]string{"message": "not found"}
					} else {
						data = map[string]any{"droplet": item}
					}
				case req.Method == "DELETE" && req.URL.Path == "/v2/droplets/77":
					deleted++
					code = 204
				default:
					return nil, fmt.Errorf("unexpected request: %s %s", req.Method, req.URL)
				}
				body, _ := json.Marshal(data)
				return &http.Response{StatusCode: code, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body)), Request: req}, nil
			})
			t.Cleanup(func() { http.DefaultTransport = original })
			log := filepath.Join(dir, "ssh.log")
			script := "#!/bin/sh\nprintf '%s\\n' \"$@\" >> \"$FIXNETSTOP_SSH_LOG\"\nexit 0\n"
			if mode == "ssh failure" {
				script = strings.Replace(script, "exit 0", "exit 255", 1)
			}
			if err := os.WriteFile(filepath.Join(dir, "ssh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir)
			t.Setenv("FIXNETSTOP_SSH_LOG", log)
			var stderr bytes.Buffer
			err := (core.App{Stdout: io.Discard, Stderr: &stderr}).Run(t.Context(), []string{"stop", "--provider", providerName, "--id", id})
			got, _ := os.ReadFile(log)
			if mode == "ownership mismatch" {
				if err == nil || deleted != 0 || len(got) != 0 {
					t.Fatalf("ownership err=%v deletes=%d ssh=%s", err, deleted, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stop=%v stderr=%s", err, &stderr)
			}
			if mode == "absent" || mode == "invalid key namespace" {
				if len(got) != 0 {
					t.Fatalf("unsafe guest cleanup: %s", got)
				}
				if mode == "invalid key namespace" && !strings.Contains(stderr.String(), "could not prepare guest SSH") {
					t.Fatalf("missing warning: %s", &stderr)
				}
				return
			}
			if deleted != 1 {
				t.Fatalf("deletes=%d stderr=%s", deleted, &stderr)
			}
			for _, want := range []string{"192.0.2.77", key, ".crabbox/actions/", "pkill -f", "tailscale logout"} {
				if !strings.Contains(string(got), want) {
					t.Errorf("missing cleanup %q in %s; stderr=%s", want, got, &stderr)
				}
			}
		})
	}
}
