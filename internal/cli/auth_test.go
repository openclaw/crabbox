package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

func TestWriteBrokerLoginReturnsWrittenBrokerDespiteEnvironmentOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CRABBOX_CONFIG", filepath.Join(home, "config.yaml"))
	t.Setenv("CRABBOX_COORDINATOR", "https://ambient.example.test")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "ambient-token")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN_COMMAND", `["printf","ambient-token"]`)

	_, cfg, err := writeBrokerLogin(
		"https://login.example.test",
		"login-token",
		"aws",
		BrokerModeManaged,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator != "https://login.example.test" || cfg.CoordToken != "login-token" {
		t.Fatalf("login verification config used ambient broker credentials: %#v", cfg)
	}
	if len(cfg.CoordTokenCommand) != 0 {
		t.Fatalf("login verification retained ambient token command: %v", cfg.CoordTokenCommand)
	}
	if cfg.Provider != "aws" || cfg.brokerProvider != "aws" {
		t.Fatalf("login verification retained ambient provider: %#v", cfg)
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

func TestFinishLoginRedactsBrokerURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "alice@example.test",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()

	const brokerURL = "https://broker-user:broker-password@broker.example.test/path?token=query-secret#fragment-secret"
	coord := &CoordinatorClient{BaseURL: server.URL, Client: server.Client()}
	for _, jsonOut := range []bool{false, true} {
		var stdout bytes.Buffer
		app := App{Stdout: &stdout, Stderr: &bytes.Buffer{}}
		if err := app.finishLogin(context.Background(), coord, "/tmp/config.yaml", Config{
			Coordinator: brokerURL,
			Provider:    "aws",
		}, jsonOut); err != nil {
			t.Fatal(err)
		}
		output := stdout.String()
		brokerOutput := output
		if jsonOut {
			var view map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &view); err != nil {
				t.Fatal(err)
			}
			brokerOutput, _ = view["broker"].(string)
		}
		if !strings.Contains(brokerOutput, "https://<redacted>@broker.example.test/path") {
			t.Fatalf("login output missing redacted broker URL: %q", output)
		}
		for _, secret := range []string{"broker-user", "broker-password", "query-secret", "fragment-secret"} {
			if strings.Contains(output, secret) {
				t.Fatalf("login output leaked %q: %q", secret, output)
			}
		}
	}
}

func TestWhoamiRedactsBrokerURL(t *testing.T) {
	clearConfigEnv(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/whoami" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorWhoami{
			Owner: "alice@example.test",
			Org:   "example-org",
			Auth:  "token",
		})
	}))
	defer server.Close()
	brokerURL := strings.Replace(server.URL, "http://", "http://broker-user:broker-password@", 1)
	t.Setenv("CRABBOX_COORDINATOR", brokerURL)

	var stdout bytes.Buffer
	if err := (App{Stdout: &stdout, Stderr: &bytes.Buffer{}}).whoami(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	output := stdout.String()
	if !strings.Contains(output, strings.Replace(server.URL, "http://", "http://<redacted>@", 1)) {
		t.Fatalf("whoami output missing redacted broker URL: %q", output)
	}
	for _, secret := range []string{"broker-user", "broker-password"} {
		if strings.Contains(output, secret) {
			t.Fatalf("whoami output leaked %q: %q", secret, output)
		}
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
	var loopbackResult = make(chan error, 1)
	const browserConfirmation = "confirm_0123456789abcdef0123456789abcdef"
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
				PollSecretHash      string `json:"pollSecretHash"`
				Provider            string `json:"provider"`
				LoopbackRedirectURI string `json:"loopbackRedirectURI"`
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
			loopbackURL, err := url.Parse(body.LoopbackRedirectURI)
			if err != nil || loopbackURL.Scheme != "http" || loopbackURL.Hostname() != "127.0.0.1" || loopbackURL.Port() == "" {
				t.Fatalf("loopback redirect URI=%q err=%v", body.LoopbackRedirectURI, err)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_test",
				URL:       "https://github.com/login/oauth/authorize?state=test",
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
			go func() {
				callback := *loopbackURL
				query := callback.Query()
				query.Set("confirmation", browserConfirmation)
				callback.RawQuery = query.Encode()
				response, err := http.Get(callback.String())
				if err == nil {
					defer response.Body.Close()
					if response.StatusCode != http.StatusOK {
						err = fmt.Errorf("loopback callback status=%d", response.StatusCode)
					}
				}
				loopbackResult <- err
			}()
		case "/v1/auth/github/poll":
			if got := r.Header.Get("CF-Access-Client-Id"); got != "access-client" {
				t.Fatalf("poll CF-Access-Client-Id=%q", got)
			}
			if got := r.Header.Get("CF-Access-Client-Secret"); got != "access-secret" {
				t.Fatalf("poll CF-Access-Client-Secret=%q", got)
			}
			var body struct {
				LoginID             string `json:"loginID"`
				PollSecret          string `json:"pollSecret"`
				BrowserConfirmation string `json:"browserConfirmation"`
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
			if body.BrowserConfirmation != browserConfirmation {
				_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginPoll{Status: "confirmation_required"})
				return
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
	if err := <-loopbackResult; err != nil {
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

func TestGitHubLoginLoopbackRejectsUnboundRequests(t *testing.T) {
	loopback, err := startGitHubLoginLoopback()
	if err != nil {
		t.Fatal(err)
	}
	defer loopback.Close()

	wrongPath, err := http.Get("http://" + loopback.listener.Addr().String() + "/wrong")
	if err != nil {
		t.Fatal(err)
	}
	wrongPath.Body.Close()
	if wrongPath.StatusCode != http.StatusNotFound {
		t.Fatalf("wrong path status=%d", wrongPath.StatusCode)
	}

	invalid, err := http.Get(loopback.URL() + "?confirmation=wrong")
	if err != nil {
		t.Fatal(err)
	}
	invalid.Body.Close()
	if invalid.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid confirmation status=%d", invalid.StatusCode)
	}

	const confirmation = "confirm_0123456789abcdef0123456789abcdef"
	valid, err := http.Get(loopback.URL() + "?confirmation=" + confirmation)
	if err != nil {
		t.Fatal(err)
	}
	valid.Body.Close()
	if valid.StatusCode != http.StatusOK {
		t.Fatalf("valid confirmation status=%d", valid.StatusCode)
	}
	if got := valid.Header.Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("referrer policy=%q", got)
	}
	select {
	case got := <-loopback.confirmations:
		if got != confirmation {
			t.Fatalf("confirmation=%q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for loopback confirmation")
	}
}

func TestGitHubLoginRejectsCompletionWithoutLoopbackConfirmation(t *testing.T) {
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
		case "/v1/auth/github/start":
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_downgrade",
				URL:       githubAuthorizeURLForTest("http://" + r.Host),
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginPoll{
				Status: "complete",
				Token:  "unbound-session-token",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	app := App{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := app.login(context.Background(), []string{"--url", server.URL, "--no-browser"})
	if err == nil || !strings.Contains(err.Error(), "before this device received") {
		t.Fatalf("error=%v", err)
	}
	cfg, loadErr := loadConfig()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if cfg.CoordToken != "" {
		t.Fatal("unbound broker token was persisted")
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

func TestGitHubLoginRejectsMismatchedRedirectOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	callbackOrigin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer callbackOrigin.Close()

	repo := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(repo, ".crabbox.yaml"),
		[]byte("broker:\n  loginRedirectOrigins:\n    - "+callbackOrigin.URL+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	var startCount int
	var pollCount int
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			startCount++
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_test",
				URL:       githubAuthorizeURLForTest(callbackOrigin.URL),
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			pollCount++
			http.Error(w, "unexpected poll", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer broker.Close()

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.login(context.Background(), []string{"--url", broker.URL, "--provider", "aws", "--no-browser"})
	if err == nil || !strings.Contains(err.Error(), "redirect_uri broker origin") {
		t.Fatalf("error=%v", err)
	}
	if startCount != 1 || pollCount != 0 {
		t.Fatalf("counts start=%d poll=%d", startCount, pollCount)
	}
	if strings.Contains(stderr.String(), "open this GitHub login URL") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout=%q", stdout.String())
	}
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Coordinator == callbackOrigin.URL || cfg.CoordToken != "" {
		t.Fatalf("unexpected config: %#v", cfg)
	}
}

func TestGitHubLoginMigratesToAllowedRedirectOrigin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	var seenPollSecretHash string
	const browserConfirmation = "confirm_0123456789abcdef0123456789abcdef"
	loopbackResult := make(chan error, 1)
	var canonicalStartCount int
	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			canonicalStartCount++
			var body struct {
				PollSecretHash      string `json:"pollSecretHash"`
				Provider            string `json:"provider"`
				LoopbackRedirectURI string `json:"loopbackRedirectURI"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Provider != "aws" {
				t.Fatalf("provider=%q", body.Provider)
			}
			seenPollSecretHash = body.PollSecretHash
			loopbackURL, err := url.Parse(body.LoopbackRedirectURI)
			if err != nil {
				t.Fatal(err)
			}
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_canonical",
				URL:       githubAuthorizeURLForTest(canonical.URL),
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
			go func() {
				callback := *loopbackURL
				query := callback.Query()
				query.Set("confirmation", browserConfirmation)
				callback.RawQuery = query.Encode()
				response, err := http.Get(callback.String())
				if err == nil {
					response.Body.Close()
				}
				loopbackResult <- err
			}()
		case "/v1/auth/github/poll":
			var body struct {
				LoginID             string `json:"loginID"`
				PollSecret          string `json:"pollSecret"`
				BrowserConfirmation string `json:"browserConfirmation"`
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
			if body.BrowserConfirmation != browserConfirmation {
				_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginPoll{Status: "confirmation_required"})
				return
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
	t.Setenv("CRABBOX_BROKER_LOGIN_REDIRECT_ORIGINS", canonical.URL)

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
			t.Fatal("poll should restart against allowed redirect origin")
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
	if err := <-loopbackResult; err != nil {
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

func TestGitHubLoginRejectsAllowedRedirectOriginThatChangesAgain(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	nextOrigin := httptest.NewServer(http.NotFoundHandler())
	defer nextOrigin.Close()

	var canonical *httptest.Server
	canonical = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/auth/github/start" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
			LoginID:   "login_canonical",
			URL:       githubAuthorizeURLForTest(nextOrigin.URL),
			ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
		})
	}))
	defer canonical.Close()
	t.Setenv("CRABBOX_BROKER_LOGIN_REDIRECT_ORIGINS", canonical.URL)

	stale := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/v1/auth/github/start" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
			LoginID:   "login_stale",
			URL:       githubAuthorizeURLForTest(canonical.URL),
			ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
		})
	}))
	defer stale.Close()

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.login(context.Background(), []string{"--url", stale.URL, "--provider", "aws", "--no-browser"})
	if err == nil || !strings.Contains(err.Error(), "does not match approved broker") {
		t.Fatalf("error=%v", err)
	}
	if strings.Contains(stderr.String(), "open this GitHub login URL") {
		t.Fatalf("stderr=%q", stderr.String())
	}
	if stdout.String() != "" {
		t.Fatalf("stdout=%q", stdout.String())
	}
}

func TestCanonicalBrokerURLFromLoginURL(t *testing.T) {
	got, ok := canonicalBrokerURLFromLoginURL("https://github.com/login/oauth/authorize?redirect_uri=https%3A%2F%2Fbroker.example.com%2Fv1%2Fauth%2Fgithub%2Fcallback&state=x")
	if !ok || got != "https://broker.example.com" {
		t.Fatalf("canonical=%q ok=%v", got, ok)
	}
}

func TestSameBrokerURLNormalizesDefaultPorts(t *testing.T) {
	tests := []struct {
		name  string
		left  string
		right string
		want  bool
	}{
		{name: "https default port", left: "https://broker.example.com", right: "https://BROKER.example.com:443/", want: true},
		{name: "http default port", left: "http://broker.example.com", right: "HTTP://broker.example.com:80", want: true},
		{name: "custom port", left: "https://broker.example.com", right: "https://broker.example.com:8443"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := sameBrokerURL(test.left, test.right); got != test.want {
				t.Fatalf("sameBrokerURL(%q, %q)=%v, want %v", test.left, test.right, got, test.want)
			}
		})
	}
}

func TestValidateGitHubLoginURL(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{name: "valid", value: "https://github.com/login/oauth/authorize?client_id=test&state=test"},
		{name: "case insensitive host", value: "https://GitHub.com/login/oauth/authorize?state=test"},
		{name: "http", value: "http://github.com/login/oauth/authorize?state=test", wantErr: true},
		{name: "other host", value: "https://github.example.com/login/oauth/authorize?state=test", wantErr: true},
		{name: "host suffix", value: "https://github.com.example.com/login/oauth/authorize?state=test", wantErr: true},
		{name: "userinfo", value: "https://github.com@evil.example/login/oauth/authorize?state=test", wantErr: true},
		{name: "custom port", value: "https://github.com:443/login/oauth/authorize?state=test", wantErr: true},
		{name: "wrong path", value: "https://github.com/login?state=test", wantErr: true},
		{name: "escaped path", value: "https://github.com/login/oauth/%61uthorize?state=test", wantErr: true},
		{name: "fragment", value: "https://github.com/login/oauth/authorize?state=test#fragment", wantErr: true},
		{name: "custom scheme", value: "itms-services://github.com/login/oauth/authorize", wantErr: true},
		{name: "local file", value: "file:///tmp/login.html", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateGitHubLoginURL(test.value)
			if test.wantErr && err == nil {
				t.Fatal("expected error")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestGitHubLoginRejectsInvalidAuthorizationURL(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("CRABBOX_CONFIG", "")
	t.Setenv("CRABBOX_COORDINATOR", "")
	t.Setenv("CRABBOX_COORDINATOR_TOKEN", "")
	t.Setenv("CRABBOX_PROVIDER", "")

	var pollCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/auth/github/start":
			_ = json.NewEncoder(w).Encode(CoordinatorGitHubLoginStart{
				LoginID:   "login_test",
				URL:       "file:///tmp/fake-login.html",
				ExpiresAt: time.Now().Add(time.Minute).Format(time.RFC3339),
			})
		case "/v1/auth/github/poll":
			pollCount++
			http.Error(w, "unexpected poll", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	app := App{Stdout: &stdout, Stderr: &stderr}
	err := app.login(context.Background(), []string{"--url", server.URL, "--no-browser"})
	if err == nil || !strings.Contains(err.Error(), "invalid authorization URL") {
		t.Fatalf("error=%v", err)
	}
	if pollCount != 0 {
		t.Fatalf("poll count=%d want 0", pollCount)
	}
	if strings.Contains(stderr.String(), "file:///") {
		t.Fatalf("stderr exposed rejected URL: %q", stderr.String())
	}
}

func githubAuthorizeURLForTest(base string) string {
	return "https://github.com/login/oauth/authorize?redirect_uri=" + url.QueryEscape(base+"/v1/auth/github/callback") + "&state=test"
}
