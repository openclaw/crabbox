package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteBrokerLoginStoresTokenInUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	path, cfg, err := writeBrokerLogin("https://crabbox.example.test", "secret", "aws", BrokerModeManaged)
	if err != nil {
		t.Fatal(err)
	}
	if path != userConfigPath() {
		t.Fatalf("path=%q want %q", path, userConfigPath())
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestWriteBrokerLoginHonorsExplicitConfigPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	explicit := filepath.Join(home, "isolated.yaml")
	t.Setenv("CRABBOX_CONFIG", explicit)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	path, cfg, err := writeBrokerLogin("https://crabbox.example.test", "secret", "aws", BrokerModeManaged)
	if err != nil {
		t.Fatal(err)
	}
	if path != explicit {
		t.Fatalf("path=%q want %q", path, explicit)
	}
	if cfg.Coordinator != "https://crabbox.example.test" || cfg.CoordToken != "secret" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestWriteBrokerLoginRejectsDirectOnlyProvider(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	_, _, err := writeBrokerLogin("https://crabbox.example.test", "secret", "xcp-ng", BrokerModeManaged)
	if err == nil || !strings.Contains(err.Error(), "cannot be used with a broker") {
		t.Fatalf("err=%v, want brokered provider rejection", err)
	}
	if _, statErr := os.Stat(configPath); !os.IsNotExist(statErr) {
		t.Fatalf("config file exists after rejected provider: %v", statErr)
	}
}

func TestWriteBrokerLoginAcceptsDirectProviderInRegisteredMode(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("broker:\n  mode: registered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, cfg, err := writeBrokerLogin("https://crabbox.example.test", "secret", "xcp-ng", BrokerModeRegistered)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrokerMode != BrokerModeRegistered || cfg.Provider != "xcp-ng" || cfg.CoordToken != "secret" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoginRejectsInvalidConfigBeforeWriting(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	original := "broker:\n  mode: invalid\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	err := app.login(context.Background(), []string{
		"--url", "https://crabbox.example.test",
		"--provider", "aws",
		"--token-stdin",
	})
	if err == nil || !strings.Contains(err.Error(), "broker.mode must be managed or registered") {
		t.Fatalf("err=%v", err)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("config changed after rejection:\n%s", data)
	}
}

func TestLoginExplicitProviderOverridesInvalidStoredProvider(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("provider: typo\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "friend@example.com",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	if err := app.login(context.Background(), []string{
		"--url", server.URL,
		"--provider", "aws",
		"--token-stdin",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "aws" || cfg.Coordinator != server.URL || cfg.CoordToken != "stdin-session-token" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoginAppliesExplicitURLBeforeRegisteredModeValidation(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("broker:\n  mode: Registered\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "friend@example.com",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	if err := app.login(context.Background(), []string{
		"--url", server.URL,
		"--token-stdin",
	}); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrokerMode != BrokerModeRegistered || cfg.Coordinator != server.URL || cfg.CoordToken != "stdin-session-token" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoginWithExplicitURLDoesNotUseTopLevelDirectProvider(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("provider: xcp-ng\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "friend@example.com",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	if err := app.login(context.Background(), []string{
		"--url", server.URL,
		"--token-stdin",
	}); err != nil {
		t.Fatal(err)
	}
	file, err := readFileConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if file.Provider != "xcp-ng" || file.Broker == nil || file.Broker.Provider != "" {
		t.Fatalf("config=%#v", file)
	}
}

func TestLoginRejectsIncompatiblePersistedBrokerProvider(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	original := "broker:\n  url: https://broker.example.test\n  mode: managed\n  provider: xcp-ng\n"
	if err := os.WriteFile(configPath, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	err := app.login(context.Background(), []string{"--token-stdin"})
	if err == nil || !strings.Contains(err.Error(), "cannot be used with a broker") {
		t.Fatalf("err=%v", err)
	}
	data, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("config changed after rejection:\n%s", data)
	}
}

func TestCoordinatorClientForLoginAppliesURLBeforeRegisteredModeValidation(t *testing.T) {
	home := t.TempDir()
	configPath := filepath.Join(home, "config.yaml")
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", configPath)
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", `["token-helper","--scope","example"]`)
	t.Setenv("CRABBOX_PROVIDER", "")
	if err := os.WriteFile(configPath, []byte("broker:\n  mode: registered\n  token: persisted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()

	client, err := coordinatorClientForLogin(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || client.BaseURL != server.URL {
		t.Fatalf("client=%#v", client)
	}
	if client.Token != "" {
		t.Fatalf("login client retained stored token")
	}
	if got := strings.Join(client.TokenCommand, "\x00"); got != "token-helper\x00--scope\x00example" {
		t.Fatalf("login token command=%q", client.TokenCommand)
	}
}

func TestGitHubLoginNoBrowserStoresReturnedToken(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	t.Setenv("CRABBOX_ACCESS_CLIENT_ID", "access-client")
	t.Setenv("CRABBOX_ACCESS_CLIENT_SECRET", "access-secret")

	var seenPollSecretHash string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			if got := r.Header.Get("CF-Access-Client-Id"); got != "access-client" {
				t.Fatalf("start CF-Access-Client-Id=%q", got)
			}
			if got := r.Header.Get("CF-Access-Client-Secret"); got != "access-secret" {
				t.Fatalf("start CF-Access-Client-Secret=%q", got)
			}
			var body struct {
				PollSecretHash string `json:"pollSecretHash"`
				Provider       string `json:"provider"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Provider != "aws" {
				t.Fatalf("provider=%q", body.Provider)
			}
			if len(body.PollSecretHash) != 64 {
				t.Fatalf("poll secret hash=%q", body.PollSecretHash)
			}
			seenPollSecretHash = body.PollSecretHash
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_test",
				URL:       "https://github.com/login/oauth/authorize?state=test",
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			if got := r.Header.Get("CF-Access-Client-Id"); got != "access-client" {
				t.Fatalf("poll CF-Access-Client-Id=%q", got)
			}
			if got := r.Header.Get("CF-Access-Client-Secret"); got != "access-secret" {
				t.Fatalf("poll CF-Access-Client-Secret=%q", got)
			}
			var body struct {
				LoginID    string `json:"loginID"`
				PollSecret string `json:"pollSecret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.LoginID != "login_test" {
				t.Fatalf("loginID=%q", body.LoginID)
			}
			if sha256Hex(body.PollSecret) != seenPollSecretHash {
				t.Fatal("poll secret did not match start hash")
			}
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginPoll{
				Status:   "complete",
				Token:    "github-session-token",
				Owner:    "friend@example.com",
				Org:      "openclaw",
				Login:    "friend",
				Provider: "aws",
			})
		case "/v1/whoami":
			if got := r.Header.Get("Authorization"); got != "Bearer github-session-token" {
				t.Fatalf("authorization=%q", got)
			}
			if got := r.Header.Get("CF-Access-Client-Id"); got != "access-client" {
				t.Fatalf("whoami CF-Access-Client-Id=%q", got)
			}
			if got := r.Header.Get("CF-Access-Client-Secret"); got != "access-secret" {
				t.Fatalf("whoami CF-Access-Client-Secret=%q", got)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
				Owner: "friend@example.com",
				Org:   "openclaw",
				Auth:  "github",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.login(context.Background(), []string{"--url", server.URL, "--provider", "aws", "--no-browser"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "open this GitHub login URL") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "user=friend@example.com") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator != server.URL || cfg.CoordToken != "github-session-token" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoginWithTokenReadsAppStdin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/whoami":
			if got := r.Header.Get("Authorization"); got != "Bearer stdin-session-token" {
				t.Fatalf("authorization=%q", got)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
				Owner: "friend@example.com",
				Org:   "openclaw",
				Auth:  "token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	app := App{
		Stdout: &stdout,
		Stderr: &stderr,
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	if err := app.login(context.Background(), []string{"--url", server.URL, "--provider", "aws", "--token-stdin"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "user=friend@example.com") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator != server.URL || cfg.CoordToken != "stdin-session-token" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestLoginWithTokenUsesEffectiveRegisteredMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "friend@example.com",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()
	t.Setenv("CRABBOX_COORDINATOR", server.URL)
	t.Setenv("CRABBOX_COORDINATOR_MODE", "Registered")
	t.Setenv("CRABBOX_PROVIDER", "xcp-ng")

	app := App{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Stdin:  strings.NewReader("stdin-session-token\n"),
	}
	if err := app.login(context.Background(), []string{"--token-stdin"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_MODE", "")
	t.Setenv("CRABBOX_PROVIDER", "")
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.BrokerMode != BrokerModeRegistered || cfg.Provider != "xcp-ng" || cfg.CoordToken != "stdin-session-token" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestGitHubLoginMigratesToCanonicalRedirectOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	var seenPollSecretHash string
	var canonicalStartCount int
	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			canonicalStartCount++
			var body struct {
				PollSecretHash string `json:"pollSecretHash"`
				Provider       string `json:"provider"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Provider != "aws" {
				t.Fatalf("provider=%q", body.Provider)
			}
			seenPollSecretHash = body.PollSecretHash
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_canonical",
				URL:       githubAuthorizeURLForTest(canonical.URL),
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			var body struct {
				LoginID    string `json:"loginID"`
				PollSecret string `json:"pollSecret"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.LoginID != "login_canonical" {
				t.Fatalf("loginID=%q", body.LoginID)
			}
			if sha256Hex(body.PollSecret) != seenPollSecretHash {
				t.Fatal("poll secret did not match canonical start hash")
			}
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginPoll{
				Status:   "complete",
				Token:    "canonical-session-token",
				Owner:    "friend@example.com",
				Org:      "openclaw",
				Login:    "friend",
				Provider: "aws",
			})
		case "/v1/whoami":
			if got := r.Header.Get("Authorization"); got != "Bearer canonical-session-token" {
				t.Fatalf("authorization=%q", got)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
				Owner: "friend@example.com",
				Org:   "openclaw",
				Auth:  "github",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer canonical.Close()

	var staleStartCount int
	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			staleStartCount++
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_stale",
				URL:       githubAuthorizeURLForTest(canonical.URL),
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			t.Fatal("poll should restart against canonical redirect origin")
		default:
			http.NotFound(w, r)
		}
	}))
	defer stale.Close()

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	if err := app.login(context.Background(), []string{"--url", stale.URL, "--provider", "aws", "--no-browser"}); err != nil {
		t.Fatal(err)
	}
	if staleStartCount != 1 || canonicalStartCount != 1 {
		t.Fatalf("start counts stale=%d canonical=%d", staleStartCount, canonicalStartCount)
	}
	if !strings.Contains(stderr.String(), "redirect_uri="+url.QueryEscape(canonical.URL+"/v1/auth/github/callback")) {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "user=friend@example.com") {
		t.Fatalf("stdout=%q", stdout.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator != canonical.URL || cfg.CoordToken != "canonical-session-token" || cfg.Provider != "aws" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestCanonicalBrokerURLFromLoginURL(t *testing.T) {
	got, ok := canonicalBrokerURLFromLoginURL("https://github.com/login/oauth/authorize?redirect_uri=https%3A%2F%2Fbroker.example.com%2Fv1%2Fauth%2Fgithub%2Fcallback&state=x")
	if !ok || got != "https://broker.example.com" {
		t.Fatalf("canonical=%q ok=%v", got, ok)
	}
}

func githubAuthorizeURLForTest(base string) string {
	return "https://github.com/login/oauth/authorize?redirect_uri=" + url.QueryEscape(base+"/v1/auth/github/callback") + "&state=test"
}
