package sprites

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openclaw/crabbox/internal/testutil"
)

type spritesSandboxedCLIRunner struct {
	cli     string
	profile string
}

func (r spritesSandboxedCLIRunner) Run(ctx context.Context, req LocalCommandRequest) (LocalCommandResult, error) {
	req.Name = "/usr/bin/sandbox-exec"
	req.Args = append([]string{"-f", r.profile, r.cli}, req.Args...)
	return (spritesRealCLIRunner{}).Run(ctx, req)
}

// This exercises native file-based credential resolution, not OS keyring access.
func TestSpritesRealCLIOverridesWorkingSavedContext(t *testing.T) {
	if os.Getenv("CRABBOX_TEST_SPRITE_CLI") != "1" {
		t.Skip("set CRABBOX_TEST_SPRITE_CLI=1 to test an installed sprite CLI against local endpoints")
	}
	cli, err := exec.LookPath("sprite")
	if err != nil {
		t.Fatal(err)
	}
	dirs := testutil.IsolateUserDirs(t)
	for _, key := range []string{"SPRITE_TOKEN", "SPRITE_URL", "SPRITES_API_URL", "SPRITE_ORG"} {
		t.Setenv(key, "")
	}

	var savedRequests, configuredRequests [2]atomic.Int32
	endpoint := func(token string, requests *[2]atomic.Int32) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("Authorization") != "Bearer "+token {
				t.Error("native CLI used the wrong credential")
			}
			if r.Method != http.MethodGet {
				t.Errorf("unexpected native request method: %s", r.Method)
			}
			switch r.URL.Path {
			case "/v1/sprites/crabbox-test/exec":
				requests[0].Add(1)
			case "/v1/sprites/crabbox-test/proxy":
				requests[1].Add(1)
			default:
				t.Errorf("unexpected native request path: %s", r.URL.Path)
			}
			http.Error(w, "test endpoint: no remote execution", http.StatusUnauthorized)
		}))
	}
	saved := endpoint("test-saved-token", &savedRequests)
	defer saved.Close()
	configured := endpoint("test-configured-token", &configuredRequests)
	defer configured.Close()

	config := map[string]any{
		"version":           "1",
		"current_selection": map[string]string{"url": saved.URL, "org": "test-org"},
		"urls": map[string]any{saved.URL: map[string]any{
			"url": saved.URL,
			"orgs": map[string]any{"test-org": map[string]any{
				"name": "test-org", "token": "test-saved-token",
				"keyring_key": "sprites:org:" + saved.URL + ":test-org",
				"sprites":     map[string]any{},
			}},
		}},
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(dirs.Home, ".sprites")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "sprites.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Force the native CLI's file fallback without touching the user's Keychain,
	// and refuse any traffic outside the two loopback endpoints.
	profile := filepath.Join(dirs.Root, "native-cli.sb")
	if err := os.WriteFile(profile, []byte(`(version 1)
(allow default)
(deny network-outbound)
(allow network-outbound (remote ip "localhost:*"))
(deny process-exec (literal "/usr/bin/security"))
`), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := spritesSandboxedCLIRunner{cli: cli, profile: profile}
	b := &spritesBackend{
		cfg: Config{Sprites: SpritesConfig{Token: "test-configured-token", APIURL: configured.URL}},
		rt:  Runtime{Exec: runner},
	}
	for i, args := range [][]string{{"exec", "-s", "crabbox-test", "--", "true"}, {"proxy", "-s", "crabbox-test", "-W", "22"}} {
		// Prove the saved fixture works before testing Crabbox's overrides.
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		_, err := runner.Run(ctx, LocalCommandRequest{Name: "sprite", Args: args, Env: os.Environ(), Dir: dirs.Home})
		cancel()
		if err == nil || savedRequests[i].Load() == 0 || configuredRequests[i].Load() != 0 {
			t.Fatalf("%s did not use the working saved context", args[0])
		}
		before := savedRequests[i].Load()
		ctx, cancel = context.WithTimeout(t.Context(), 5*time.Second)
		_, err = b.runSprite(ctx, args, nil, nil)
		cancel()
		if err == nil || configuredRequests[i].Load() == 0 || savedRequests[i].Load() != before {
			t.Fatalf("%s did not override the saved context", args[0])
		}
	}
}
