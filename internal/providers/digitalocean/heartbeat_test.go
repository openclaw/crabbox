package digitalocean

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
)

type heartbeatTransport func(*http.Request) (*http.Response, error)

func (f heartbeatTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFixedDropletHeartbeatAndStatusWait(t *testing.T) {
	for _, command := range []string{"heartbeat", "status"} {
		for _, change := range []string{"owned", "ordinary", "scope", "resource", "missing resource", "account", "missing account"} {
			t.Run(command+"/"+change, func(t *testing.T) {
				api := &fakeDigitalOceanAPI{}
				b := newTestBackend(t, api)
				repo := t.TempDir()
				t.Chdir(repo)
				req := core.AcquireRequest{RequestedLeaseID: "cbx_abcdef123456", RequestedSlug: "fixed-heartbeat", Repo: core.Repo{Root: repo}, Keep: true}
				scope := "team:test-account"
				if change == "ordinary" {
					req.RequestedLeaseID, scope = "", ""
				}
				lease, err := b.Acquire(t.Context(), req)
				if err != nil {
					t.Fatal(err)
				}
				claim, err := core.ReadLeaseClaim(lease.LeaseID)
				if err != nil || claim.CloudID != lease.Server.CloudID || claim.CloudID == "" || claim.ProviderScope != scope || claim.RepoRoot != repo {
					t.Fatalf("fixed acquisition lost ownership: %+v, %v", claim, err)
				}
				if err := core.WithDurableLeaseClaimLock(lease.LeaseID, func(c *core.LeaseClaim, _ bool, persist func() error) error {
					switch change {
					case "scope":
						c.ProviderScope = "team:foreign"
					case "resource":
						c.CloudID = "999"
					case "missing resource":
						c.CloudID = ""
					case "missing account":
						delete(c.Labels, digitalOceanAccountLabel)
					}
					return persist()
				}); err != nil {
					t.Fatal(err)
				}
				config := filepath.Join(t.TempDir(), "config.yaml")
				if err := os.WriteFile(config, []byte("provider: digitalocean\n"), 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("CRABBOX_CONFIG", config)
				t.Setenv("DIGITALOCEAN_TOKEN", "synthetic-heartbeat-token")
				for _, name := range []string{"CRABBOX_COORDINATOR", "CRABBOX_COORDINATOR_MODE", "CRABBOX_COORDINATOR_TOKEN"} {
					t.Setenv(name, "")
				}
				item := api.created[0]
				labels := labelsFromTags(item.Tags)
				if command == "status" {
					labels["state"] = "provisioning"
				}
				item.Tags = tagsFromLabels(labels)
				writes := 0
				previous := http.DefaultTransport
				t.Cleanup(func() { http.DefaultTransport = previous })
				http.DefaultTransport = heartbeatTransport(func(r *http.Request) (*http.Response, error) {
					var body any
					switch {
					case r.Method == "GET" && r.URL.Path == "/v2/account":
						account := "test-account"
						if change == "account" {
							account = "foreign"
						}
						body = map[string]any{"account": map[string]any{"team": map[string]string{"uuid": account}}}
					case r.Method == "GET" && r.URL.Path == "/v2/droplets/100":
						body = map[string]any{"droplet": item}
					case r.Method == "GET" && r.URL.Path == "/v2/droplets":
						body = map[string]any{"droplets": []droplet{item}}
					case r.Method == "POST" && r.URL.Path == "/v2/tags":
						var tag map[string]string
						if err := json.NewDecoder(r.Body).Decode(&tag); err != nil {
							return nil, err
						}
						body = map[string]any{"tag": tag}
						writes++
					case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v2/tags/"):
						body = map[string]any{"tag": map[string]string{"name": strings.TrimPrefix(r.URL.Path, "/v2/tags/")}}
					case (r.Method == "POST" || r.Method == "DELETE") && strings.HasPrefix(r.URL.Path, "/v2/tags/"):
						writes++
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL)
						return nil, fmt.Errorf("unexpected request: %s %s", r.Method, r.URL)
					}
					data, err := json.Marshal(body)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, err
				})
				identifier := lease.LeaseID
				if change == "scope" || change == "resource" {
					identifier = claim.Slug
				}
				args := []string{command, "--provider", providerName, "--id", identifier, "--json"}
				if command == "heartbeat" {
					args = append(args, "--idle-timeout", "45m")
				} else {
					args = append(args, "--wait", "--wait-timeout", "100ms")
				}
				var stdout, stderr bytes.Buffer
				err = (core.App{Stdout: &stdout, Stderr: &stderr}).Run(t.Context(), args)
				if change == "owned" || change == "ordinary" {
					if writes == 0 || (command == "heartbeat" && err != nil) {
						t.Fatalf("owned lease not touched: writes=%d err=%v stderr=%s", writes, err, &stderr)
					}
					if command == "heartbeat" {
						var result struct {
							IdleTimeout string `json:"idleTimeout"`
						}
						if err := json.Unmarshal(stdout.Bytes(), &result); err != nil || result.IdleTimeout != "45m0s" {
							t.Fatalf("idle policy not refreshed: %s (%v)", &stdout, err)
						}
					}
				} else if writes != 0 || (command == "heartbeat" && err == nil) {
					t.Fatalf("foreign/incomplete claim touched: writes=%d err=%v", writes, err)
				}
			})
		}
	}
}
