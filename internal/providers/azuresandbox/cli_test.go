package azuresandbox

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestSandboxCLIWorkerLifecycle(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("CRABBOX_PROVIDER", providerName)
	dir := t.TempDir()
	t.Chdir(dir)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	for key, value := range map[string]string{
		"CRABBOX_AZURE_SANDBOX_REGION": "westus3", "CRABBOX_AZURE_SANDBOX_SUBSCRIPTION_ID": "subscription",
		"CRABBOX_AZURE_SANDBOX_RESOURCE_GROUP": "workers", "CRABBOX_AZURE_SANDBOX_GROUP": "sandboxes",
		"BOOTSTRAP_TOKEN": "ambient-not-selected",
	} {
		t.Setenv(key, value)
	}
	f := &fixture{}
	commands := 0
	uploads := 0
	c := fixtureClient(t, func(r *http.Request) (*http.Response, error) {
		if strings.HasSuffix(r.URL.Path, "/files") {
			data, err := io.ReadAll(r.Body)
			if err != nil || string(data) != "repository-pack" || r.URL.Query().Get("path") != "/workspace/repository.pack" {
				t.Fatal("repository upload changed")
			}
			uploads++
			return response(200, ""), nil
		}
		if !strings.HasSuffix(r.URL.Path, "/executeShellCommand") {
			return f.request(t, r)
		}
		commands++
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if commands == 1 && (!strings.Contains(body["command"], "synthetic-token") || !strings.Contains(body["command"], "bootstrap-marker")) {
			t.Fatal("CLI lost forwarded environment or stdin script")
		}
		return response(200, `{"exitCode":0,"stdout":"ready","stderr":""}`), nil
	})
	previous := newAPI
	newAPI = func(core.Config, core.Runtime) (*client, error) { return c, nil }
	t.Cleanup(func() { newAPI = previous })
	var stdout, stderr bytes.Buffer
	app := core.App{Stdout: &stdout, Stderr: &stderr, Stdin: strings.NewReader("echo bootstrap-marker")}
	lease := "cbx_abcdef123456"
	run := func(args ...string) {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		if err := app.Run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v; stderr=%s", args, err, stderr.String())
		}
	}
	run("warmup", "--provider", providerName, "--class", "small", "--ttl", "4h", "--idle-timeout", "15m", "--lease-id", lease, "--keep", "--network", "public", "--tailscale=false")
	run("inspect", "--provider", providerName, "--id", lease, "--json")
	var state core.StatusView
	if err := json.Unmarshal(stdout.Bytes(), &state); err != nil || !state.Ready || state.ServerID != "unique-resource" {
		t.Fatalf("inspect: %s %v", stdout.String(), err)
	}
	// Exercise the user-facing slug published by warmup through every command.
	lease = state.Slug
	run("inspect", "--provider", providerName, "--id", lease, "--json")
	run("list", "--provider", providerName)
	if !strings.Contains(stdout.String(), "lease="+state.ID) || !strings.Contains(stdout.String(), "slug="+state.Slug) || !strings.Contains(stdout.String(), "keep=true target=linux") {
		t.Fatalf("list omitted usable identity: %s", stdout.String())
	}
	if _, ok := f.box.Labels["lease"]; ok {
		t.Fatal("list projection modified remote ownership labels")
	}
	profile := filepath.Join(dir, "private.env")
	if err := os.WriteFile(profile, []byte("BOOTSTRAP_TOKEN=synthetic-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("run", "--provider", providerName, "--network", "public", "--tailscale=false", "--id", lease, "--keep=true", "--no-sync", "--allow-env", "BOOTSTRAP_TOKEN", "--env-from-profile", profile, "--script-stdin")
	if strings.Contains(stderr.String(), "synthetic-token") {
		t.Fatal("forwarded token leaked")
	}
	pack := filepath.Join(dir, "repository.pack")
	if err := os.WriteFile(pack, []byte("repository-pack"), 0600); err != nil {
		t.Fatal(err)
	}
	run("cp", "--provider", providerName, "--id", lease, pack, "SANDBOX:/workspace/repository.pack")
	other := t.TempDir()
	t.Chdir(other)
	if out, err := exec.Command("git", "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if err := app.Run(t.Context(), []string{"cp", "--provider", providerName, "--id", lease, pack, "SANDBOX:/workspace/repository.pack"}); err == nil {
		t.Fatal("another repository uploaded through this lease")
	}
	if uploads != 1 {
		t.Fatal("denied upload reached the Sandbox")
	}
	t.Chdir(dir)
	run("heartbeat", "--provider", providerName, "--id", lease, "--json")
	run("stop", "--provider", providerName, "--id", lease)
	if f.creates != 1 || f.deletes != 1 || commands != 2 || uploads != 1 {
		t.Fatalf("unexpected effects: %+v commands=%d", f, commands)
	}
}
