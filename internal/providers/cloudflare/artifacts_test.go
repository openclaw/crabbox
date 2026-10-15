package cloudflare

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
)

func serveCloudflareArtifact(w http.ResponseWriter, archive []byte) {
	digest := sha256.Sum256(archive)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Length", strconv.Itoa(len(archive)))
	w.Header().Set("X-Crabbox-Artifact-Protocol", cloudflareArtifactProtocol)
	w.Header().Set("X-Crabbox-Artifact-Sha256", hex.EncodeToString(digest[:]))
	_, _ = w.Write(archive)
}

func TestCloudflareArtifactCompatibilityPrecedesAllocation(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.URL.Path)
		_, _ = io.WriteString(w, `{"ok":true,"runner":"cloudflare"}`)
	}))
	defer server.Close()
	cfg := core.Config{Cloudflare: core.CloudflareConfig{APIURL: server.URL, Token: "fixture-token"}}
	b := cloudflareBackend{cfg: cfg, rt: core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: io.Discard}}
	_, err := b.Run(context.Background(), core.RunRequest{NoSync: true, Command: []string{"true"}, ArtifactGlobs: []string{"reports/**"}})
	if err == nil || !strings.Contains(err.Error(), "update the deployed Cloudflare runner") || strings.Join(requests, ",") != "/v1/readiness" {
		t.Fatalf("requests=%v err=%v", requests, err)
	}
}

func TestCloudflareArtifactTransferValidation(t *testing.T) {
	for _, kind := range []string{"digest", "truncated", "oversized", "protocol", "redaction"} {
		t.Run(kind, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing authentication")
				}
				if kind == "redaction" {
					http.Error(w, "Bearer fixture-token", 422)
					return
				}
				w.Header().Set("Content-Type", "application/gzip")
				w.Header().Set("Content-Length", "12")
				w.Header().Set("X-Crabbox-Artifact-Protocol", cloudflareArtifactProtocol)
				w.Header().Set("X-Crabbox-Artifact-Sha256", strings.Repeat("0", 64))
				if kind == "oversized" {
					w.Header().Set("Content-Length", strconv.FormatInt(core.DelegatedRunArtifactDefaultMaxBytes+1, 10))
				}
				if kind == "protocol" {
					w.Header().Set("X-Crabbox-Artifact-Protocol", "unknown")
				}
				if kind == "truncated" {
					_, _ = io.WriteString(w, "short")
					return
				}
				_, _ = io.WriteString(w, "123456789012")
			}))
			defer server.Close()
			client, err := newCloudflareClient(core.Config{Cloudflare: core.CloudflareConfig{APIURL: server.URL, Token: "fixture-token"}}, core.Runtime{HTTP: server.Client()})
			if err != nil {
				t.Fatal(err)
			}
			data, err := client.collectArtifacts(context.Background(), "cbx_test", "/workspace/repo", "collector")
			if err == nil || data != nil || strings.Contains(err.Error(), "fixture-token") {
				t.Fatalf("data=%d error=%v", len(data), err)
			}
		})
	}
}

func TestCloudflareArtifactsFreshAndReusedFailedCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX collector fixture")
	}
	for _, reused := range []bool{false, true} {
		t.Run(fmt.Sprintf("reused=%t", reused), func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", t.TempDir())
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "result.json"), []byte("{\"ok\":false}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var events []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("missing authentication")
				}
				switch {
				case r.URL.Path == "/v1/readiness":
					_, _ = fmt.Fprintf(w, `{"ok":true,"runner":"cloudflare","artifactProtocol":%q}`, cloudflareArtifactProtocol)
				case r.Method == http.MethodDelete:
					events = append(events, "cleanup")
					_, _ = io.WriteString(w, `{}`)
				case r.URL.Path == "/v1/sandboxes":
					var create createSandboxRequest
					_ = json.NewDecoder(r.Body).Decode(&create)
					_ = json.NewEncoder(w).Encode(cloudflareContainer{ID: create.ID, State: "running", Workdir: root})
				case strings.HasSuffix(r.URL.Path, "/exec-stream"):
					var command struct {
						Command string `json:"command"`
					}
					_ = json.NewDecoder(r.Body).Decode(&command)
					code := 0
					if command.Command == "'false'" {
						code = 23
						events = append(events, "command")
					}
					w.Header().Set("Content-Type", "application/x-ndjson")
					_, _ = fmt.Fprintf(w, "{\"type\":\"complete\",\"exitCode\":%d}\n", code)
				case strings.HasSuffix(r.URL.Path, "/artifacts"):
					events = append(events, "artifacts")
					var collect struct{ Cwd, Script, Protocol string }
					_ = json.NewDecoder(r.Body).Decode(&collect)
					if collect.Cwd != root || collect.Protocol != cloudflareArtifactProtocol {
						t.Errorf("collect scope=%+v", collect)
					}
					archive := filepath.Join(root, "transfer.tgz")
					cmd := exec.Command("bash", "-c", collect.Script, "collector", archive)
					cmd.Dir = root
					if output, err := cmd.CombinedOutput(); err != nil {
						t.Errorf("collector: %v %s", err, output)
						http.Error(w, "collector failed", 422)
						return
					}
					data, _ := os.ReadFile(archive)
					serveCloudflareArtifact(w, data)
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			cfg := core.Config{TTL: time.Minute, Cloudflare: core.CloudflareConfig{APIURL: server.URL, Token: "fixture-token", Workdir: root}}
			var stderr bytes.Buffer
			b := cloudflareBackend{cfg: cfg, rt: core.Runtime{HTTP: server.Client(), Stdout: io.Discard, Stderr: &stderr}}
			local := filepath.Join(t.TempDir(), "result.json")
			req := core.RunRequest{Repo: core.Repo{Root: root}, NoSync: true, Command: []string{"false"}, Downloads: []string{"result.json=" + local}, RequiredArtifactGlobs: []string{"*.json"}}
			if reused {
				if err := core.ClaimLeaseForRepoProvider("cbx_test", "fixture", providerName, root, time.Hour, false); err != nil {
					t.Fatal(err)
				}
				req.ID = "cbx_test"
			}
			result, err := b.Run(context.Background(), req)
			if err == nil || result.ExitCode != 23 || result.ErrorKind != core.RunErrorCommandExit || len(result.Artifacts) != 1 {
				t.Fatalf("result=%+v err=%v stderr=%s", result, err, &stderr)
			}
			got, err := os.ReadFile(local)
			if err != nil || string(got) != "{\"ok\":false}\n" {
				t.Fatalf("download=%q err=%v", got, err)
			}
			want := "command,artifacts,cleanup"
			if reused {
				want = "command,artifacts"
			}
			if strings.Join(events, ",") != want {
				t.Fatalf("events=%v want=%s", events, want)
			}
		})
	}
}
