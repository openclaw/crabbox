package sprites

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	core "github.com/openclaw/crabbox/internal/cli"
	"github.com/openclaw/crabbox/internal/testutil"
)

func TestSpritesTransportUsesResolvedEnvironment(t *testing.T) {
	testutil.IsolateUserDirs(t)
	t.Setenv("SPRITE_TOKEN", "ambient-token")
	t.Setenv("SPRITE_URL", "https://ambient.example")
	t.Setenv("SPRITES_API_URL", "https://other.example")
	t.Setenv("CRABBOX_SPRITES_TOKEN", "alias-token")
	runner := &recordingRunner{}
	b := &spritesBackend{cfg: Config{Provider: spritesProvider, Sprites: SpritesConfig{Token: " resolved-token ", APIURL: "https://API.SPRITES.DEV:443/"}}, rt: Runtime{Exec: runner, Stderr: io.Discard}}
	if err := b.bootstrapSSH(t.Context(), "crabbox-test", "ssh-ed25519 AAAAtest"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SPRITE_TOKEN": "resolved-token", "SPRITE_URL": "https://api.sprites.dev", "SPRITES_API_URL": "https://api.sprites.dev"}
	for key, value := range want {
		count := 0
		for _, entry := range runner.requests[0].Env {
			name, got, _ := strings.Cut(entry, "=")
			if strings.EqualFold(name, key) {
				count++
				if got != value {
					t.Errorf("unexpected %s override", key)
				}
			}
		}
		if count != 1 {
			t.Errorf("%s occurred %d times", key, count)
		}
	}
	if strings.Contains(strings.Join(runner.requests[0].Args, " "), "resolved-token") {
		t.Fatal("credential leaked into bootstrap argv")
	}
	target, err := b.sshTarget("crabbox-test", "/tmp/test-key")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(target.ChildEnv, want) || !target.NoControlMaster {
		t.Fatal("SSH proxy does not use pinned credentials/endpoint and a fresh connection")
	}
	if strings.Contains(target.ProxyCommand, "resolved-token") {
		t.Fatal("credential leaked into proxy argv")
	}
	encoded, err := json.Marshal(core.LeaseTarget{SSH: target})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "resolved-token") || strings.Contains(string(encoded), `"ChildEnv":`) {
		t.Fatal("transport credential serialized in lease target")
	}
	server := Server{Provider: spritesProvider, CloudID: "crabbox-test", Name: "crabbox-test", Labels: map[string]string{"name": "crabbox-test"}}
	if err := core.ClaimLeaseTargetForRepoConfig("cbx_transport", "transport", b.cfg, server, target, t.TempDir(), 0, false); err != nil {
		t.Fatal(err)
	}
	claim, _, err := core.ReadLeaseClaimWithPresence("cbx_transport")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err = json.Marshal(claim)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "resolved-token") {
		t.Fatal("transport credential persisted in claim")
	}
}

func TestSpritesTransportRejectsInvalidEndpointBeforeExec(t *testing.T) {
	runner := &recordingRunner{}
	b := &spritesBackend{cfg: Config{Sprites: SpritesConfig{APIURL: "http://not-loopback.example"}}, rt: Runtime{Exec: runner}}
	if _, err := b.runSprite(t.Context(), []string{"--version"}, nil, nil); err == nil {
		t.Fatal("expected URL validation error")
	}
	if len(runner.calls) != 0 {
		t.Fatal("CLI invoked with invalid endpoint")
	}
}

type spritesRealCLIRunner struct{}

func (spritesRealCLIRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	cmd := exec.CommandContext(ctx, req.Name, req.Args...)
	cmd.Env = req.Env
	cmd.Dir = req.Dir
	output, err := cmd.CombinedOutput()
	code := 1
	if cmd.ProcessState != nil {
		code = cmd.ProcessState.ExitCode()
	}
	return LocalCommandResult{ExitCode: code, Stderr: string(output)}, err
}

// Optional, credential-free contract test against an installed sprite CLI.
// Every endpoint and token is local/fake; no account or cloud resources are used.
func TestSpritesRealCLIUsesConfiguredEndpointAndToken(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_SPRITE_CLI") != "1" {
		t.Skip("set CRABBOX_TEST_SPRITE_CLI=1 to test an installed sprite CLI against local endpoints")
	}
	if _, err := exec.LookPath("sprite"); err != nil {
		t.Fatal(err)
	}
	testutil.IsolateUserDirs(t)
	var execRequests, proxyRequests, wrongRequests atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-configured-token" {
			t.Error("wrong credential used by API or CLI")
		}
		if r.Method != http.MethodGet {
			t.Errorf("unexpected native request method: %s", r.Method)
		}
		switch r.URL.Path {
		case "/v1/sprites/crabbox-test":
			io.WriteString(w, `{"id":"test-id","name":"crabbox-test"}`)
			return
		case "/v1/sprites/crabbox-test/exec":
			execRequests.Add(1)
		case "/v1/sprites/crabbox-test/proxy":
			proxyRequests.Add(1)
		default:
			t.Errorf("unexpected native request path: %s", r.URL.Path)
		}
		http.Error(w, "test endpoint: no remote execution", http.StatusUnauthorized)
	}))
	defer api.Close()
	wrong := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrongRequests.Add(1)
		http.Error(w, "wrong endpoint", http.StatusUnauthorized)
	}))
	defer wrong.Close()
	t.Setenv("SPRITE_TOKEN", "test-ambient-token")
	t.Setenv("SPRITE_URL", wrong.URL)
	t.Setenv("SPRITES_API_URL", wrong.URL)
	t.Setenv("CRABBOX_SPRITES_TOKEN", "test-configured-token")
	t.Setenv("CRABBOX_SPRITES_API_URL", api.URL)
	cfg := Config{Sprites: SpritesConfig{Token: "test-configured-token", APIURL: api.URL}}
	client, err := newSpritesClient(cfg, Runtime{HTTP: api.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.GetSprite(t.Context(), "crabbox-test"); err != nil {
		t.Fatal(err)
	}
	b := &spritesBackend{cfg: cfg, rt: Runtime{Exec: spritesRealCLIRunner{}}}
	for _, args := range [][]string{{"exec", "-s", "crabbox-test", "--", "true"}, {"proxy", "-s", "crabbox-test", "-W", "22"}} {
		requests := &execRequests
		if args[0] == "proxy" {
			requests = &proxyRequests
		}
		before := requests.Load()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := b.runSprite(ctx, args, nil, nil)
		cancel()
		if err == nil {
			t.Fatal("mock endpoint should refuse remote execution")
		}
		if requests.Load() <= before {
			t.Fatalf("%s did not contact its configured endpoint path", args[0])
		}
	}
	if wrongRequests.Load() != 0 {
		t.Fatal("CLI contacted ambient endpoint")
	}
}
